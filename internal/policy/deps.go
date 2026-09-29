package policy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/detect"
)

// Dependency is one third-party dependency a manifest in the workspace
// declares.
//
// Only the name is kept, never the version. A policy that forbade a version
// change would fire on every `go get -u` and on every lockfile refresh, which
// is not what "no new dependencies" means to the person who wrote it: the
// risk being guarded against is code from somewhere nobody has looked at, and
// a newer release of something already trusted is a different decision.
type Dependency struct {
	// Manifest is the file that declares it, relative to the workspace and
	// slash-separated, so a monorepo's two package.json files are two
	// different places to look.
	Manifest string `json:"manifest"`
	// Name is the dependency as its ecosystem names it:
	// "github.com/spf13/cobra", "react", "requests".
	Name string `json:"name"`
}

// Baseline is what the workspace declared when the run was created: the
// record no rule can reconstruct once the agent has edited the tree.
//
// It is captured once, before anything is spent, and pinned in the run's
// manifest beside the rules it belongs to. A run that has no baseline has no
// policy, and the gate says so rather than judging the change against a
// belay.yaml somebody wrote after the run began — see ADR-0014.
type Baseline struct {
	// Dependencies is every dependency the workspace's manifests declared,
	// sorted by manifest and then by name, and deduplicated. Empty, never
	// nil, for a workspace that declares none.
	Dependencies []Dependency `json:"dependencies"`
}

// manifestKinds are the dependency manifests Survey reads.
//
// pyproject.toml is deliberately absent. Reading it properly means a TOML
// parser and PEP 621's several ways of spelling a dependency, and a parser
// that got it half right would report dependencies a project does not have or
// miss ones it does — either of which is worse than saying plainly that the
// file is not covered. The documented workaround is to name pyproject.toml in
// policy.forbidden_paths, which stops the change belay cannot judge instead of
// waving it through.
var manifestKinds = map[string]func(string) ([]string, error){
	"go.mod":           goModDependencies,
	"package.json":     packageJSONDependencies,
	"requirements.txt": requirementsDependencies,
}

// Survey records what dir's manifests declare right now.
//
// It walks the tree the same way internal/detect does — the same skipped
// directory names and the same depth — so the manifests a policy is measured
// against are the ones belay already considers part of the project, rather
// than a second, differently-drawn boundary a person would have to learn.
//
// A manifest it cannot parse is an error, not an empty result. A baseline
// missing the dependencies a file declares would make every one of them look
// new at the gate, and a run would be stopped for a dependency that was
// already there.
func Survey(ctx context.Context, dir string) (Baseline, error) {
	found := []Dependency{}
	skip := detect.DefaultSkipDirs()

	err := filepath.WalkDir(dir, func(abs string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, rerr := filepath.Rel(dir, abs)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			switch {
			case rel == ".":
				return nil
			case slices.Contains(skip, entry.Name()), strings.Count(rel, "/")+1 > detect.DefaultMaxDepth:
				return fs.SkipDir
			}
			return nil
		}
		parse, known := manifestKinds[entry.Name()]
		if !known || !entry.Type().IsRegular() {
			return nil
		}
		names, perr := parse(abs)
		if perr != nil {
			return fmt.Errorf("policy: read the dependencies %s declares: %w", rel, perr)
		}
		for _, name := range names {
			found = append(found, Dependency{Manifest: rel, Name: name})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ctx.Err()) && ctx.Err() != nil {
			return Baseline{}, fmt.Errorf("policy: survey %s: %w", dir, err)
		}
		return Baseline{}, fmt.Errorf("policy: survey the dependencies declared in %s: %w", dir, err)
	}
	return Baseline{Dependencies: normalize(found)}, nil
}

// normalize sorts and deduplicates, so two surveys of an unchanged tree are
// the same value and a stored baseline does not churn between saves.
func normalize(deps []Dependency) []Dependency {
	slices.SortFunc(deps, func(a, b Dependency) int {
		if c := strings.Compare(a.Manifest, b.Manifest); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return slices.Compact(deps)
}

// newDependencies surveys dir now and returns what it declares that base did
// not, in the order Survey reports them.
//
// It compares on (manifest, name). A dependency moved from one manifest to
// another therefore reads as new, which is the conservative reading and the
// right one: in a monorepo, pulling a package into a second service is a
// second service taking on that code.
func newDependencies(ctx context.Context, base Baseline, dir string) ([]Dependency, error) {
	now, err := Survey(ctx, dir)
	if err != nil {
		return nil, err
	}
	had := make(map[Dependency]bool, len(base.Dependencies))
	for _, dep := range base.Dependencies {
		had[dep] = true
	}
	var added []Dependency
	for _, dep := range now.Dependencies {
		if !had[dep] {
			added = append(added, dep)
		}
	}
	return added, nil
}

// goModDependencies returns the module paths a go.mod requires.
//
// It reads the require directives by hand rather than through
// golang.org/x/mod, which belay does not depend on and would not add for
// this. The grammar being read is small and fixed: a require line, or a
// require block of them, each starting with a module path. Everything after
// the path — the version, an // indirect comment — is ignored, and a line
// that does not look like a requirement is skipped rather than guessed at.
func goModDependencies(path string) ([]string, error) {
	return scanManifest(path, func(line string, inBlock *bool) string {
		switch {
		case *inBlock && line == ")":
			*inBlock = false
			return ""
		case *inBlock:
			return modulePath(line)
		case line == "require (":
			*inBlock = true
			return ""
		case strings.HasPrefix(line, "require "):
			return modulePath(strings.TrimPrefix(line, "require "))
		default:
			return ""
		}
	})
}

// modulePath returns the module path a require line starts with, or "" for a
// line that carries none.
func modulePath(line string) string {
	if line == "" || strings.HasPrefix(line, "//") {
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "//") {
		return ""
	}
	return fields[0]
}

// packageJSONDependencies returns the packages a package.json depends on, of
// every kind it can depend on them in.
//
// devDependencies count. A test framework or a build plugin is code that runs
// on a developer's machine and in CI with the same privileges as anything
// else, and a policy that stopped a new runtime dependency while waving
// through a new build-time one would be guarding the smaller half.
func packageJSONDependencies(path string) ([]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the path came from a directory walk belay performed.
	if err != nil {
		return nil, err
	}
	var doc struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var names []string
	for _, group := range []map[string]string{
		doc.Dependencies, doc.DevDependencies, doc.PeerDependencies, doc.OptionalDependencies,
	} {
		for name := range group {
			names = append(names, name)
		}
	}
	return names, nil
}

// requirementsDependencies returns the distributions a requirements.txt
// names.
//
// A line naming another file (-r, -c) or an option (--index-url) is not a
// dependency and is skipped; the file it names is surveyed on its own if the
// walk reaches it. An editable or URL requirement is recorded as written,
// which is the honest reading: belay cannot resolve what a URL installs, and
// the string itself changing is exactly the event the rule is about.
func requirementsDependencies(path string) ([]string, error) {
	return scanManifest(path, func(line string, _ *bool) string {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			return ""
		}
		line, _, _ = strings.Cut(line, "#")
		name := strings.TrimSpace(line)
		if name == "" {
			return ""
		}
		return distributionName(name)
	})
}

// scanManifest reads path line by line, handing each trimmed line to take,
// and collects every non-empty name it returns. inBlock is take's own scratch
// flag, for a grammar with a block form.
func scanManifest(path string, take func(line string, inBlock *bool) string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec // the path came from a directory walk belay performed.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var (
		names   []string
		inBlock bool
		scanner = bufio.NewScanner(f)
	)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() {
		if name := take(strings.TrimSpace(scanner.Text()), &inBlock); name != "" {
			names = append(names, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// distributionName is the part of a requirement line that names the
// distribution: everything before the first version specifier, extras
// bracket, environment marker or space.
//
// It is normalized as PEP 503 says two names are equivalent -- lowercased,
// with runs of ".", "-" and "_" folded to a single "-" -- so that
// "Flask_SQLAlchemy" and "flask-sqlalchemy" are one dependency and not a new
// one appearing the first time somebody retypes it.
func distributionName(line string) string {
	end := strings.IndexFunc(line, func(r rune) bool {
		return strings.ContainsRune("=<>!~;[ \t,@", r)
	})
	if end >= 0 {
		line = line[:end]
	}
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(line)) {
		if r == '.' || r == '-' || r == '_' {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteRune(r)
	}
	return b.String()
}
