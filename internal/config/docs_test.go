package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

// docsPath is docs/config.md relative to this package.
const docsPath = "../../docs/config.md"

// yamlBlocks returns every fenced ```yaml block in src, with the 1-based line
// number its fence opened on, so a failure names a place a human can go.
func yamlBlocks(src string) []struct {
	Line int
	Body string
} {
	var out []struct {
		Line int
		Body string
	}
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```yaml" {
			continue
		}
		start := i + 1
		var body []string
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			body = append(body, lines[i])
		}
		out = append(out, struct {
			Line int
			Body string
		}{start, strings.Join(body, "\n")})
	}
	return out
}

// Documentation that does not load is worse than no documentation: a reader
// copies it, belay rejects it, and the reader concludes the tool is broken.
// This walks every YAML example in docs/config.md through the real loader and
// validator, so the docs cannot drift from the schema without failing here.
func TestDocumentedExamplesActuallyLoad(t *testing.T) {
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Skipf("docs/config.md unavailable: %v", err)
	}
	blocks := yamlBlocks(string(raw))
	if len(blocks) == 0 {
		t.Fatal("no ```yaml blocks found in docs/config.md; this guard would silently pass forever")
	}
	for _, b := range blocks {
		if !strings.Contains(b.Body, "version:") {
			continue // a fragment illustrating one section, not a whole file
		}
		t.Run("line_"+itoa(b.Line), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "belay.yaml")
			if err := os.WriteFile(path, []byte(b.Body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(Options{Path: path})
			if err != nil {
				t.Fatalf("docs/config.md:%d does not load:\n%v\n\n--- block ---\n%s", b.Line, err, b.Body)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("docs/config.md:%d loads but fails validation:\n%v", b.Line, err)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// documentedTypes maps the words docs/config.md uses in its "**Type**:" lines
// onto the Go types that satisfy them.
var documentedTypes = map[string]func(reflect.Type) bool{
	"Boolean":           kindIs(reflect.Bool),
	"Integer":           kindIs(reflect.Int, reflect.Int64),
	"Float":             kindIs(reflect.Float64),
	"Number":            kindIs(reflect.Float64),
	"String":            kindIs(reflect.String),
	"String (duration)": func(t reflect.Type) bool { return t == reflect.TypeFor[Duration]() },
	"List of strings":   sliceOf(reflect.String),
	"List of rules":     sliceOf(reflect.Struct),
}

// kindIs matches a type whose kind is one of kinds.
func kindIs(kinds ...reflect.Kind) func(reflect.Type) bool {
	return func(t reflect.Type) bool { return slices.Contains(kinds, t.Kind()) }
}

// sliceOf matches a list whose elements are of kind elem.
func sliceOf(elem reflect.Kind) func(reflect.Type) bool {
	return func(t reflect.Type) bool { return t.Kind() == reflect.Slice && t.Elem().Kind() == elem }
}

// configPaths walks Config and returns every leaf setting by its dotted yaml
// path, with its Go type, and every section (a nested mapping such as
// review.sonar) by its path. Duration is a leaf even though it is a struct:
// it is documented as the string a user writes, not as its internals. A list
// is a leaf too: its elements are described in the prose under its heading.
func configPaths() (leaves map[string]reflect.Type, sections map[string]bool) {
	leaves, sections = map[string]reflect.Type{}, map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			if f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeFor[Duration]() {
				sections[path] = true
				walk(f.Type, path)
				continue
			}
			leaves[path] = f.Type
		}
	}
	walk(reflect.TypeFor[Config](), "")
	return leaves, sections
}

// readFieldDocs parses docs/config.md, skipping the test when the file is not
// there to read.
func readFieldDocs(t *testing.T) map[string]fieldDoc {
	t.Helper()
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Skipf("docs/config.md unavailable: %v", err)
	}
	return parseFieldDocs(string(raw))
}

// Every setting a user can write must have a heading in docs/config.md, at any
// level, and every heading that names a setting must name one that exists.
//
// The first direction is what a new field needs: the loader accepts it the
// moment it is added to Config, so without this a setting ships that nobody
// can discover except by reading the source. The second catches the rename
// or removal that leaves a heading documenting a key the strict loader now
// rejects. A heading counts as naming a setting when its path starts with a
// top-level key; headings such as `ANTHROPIC_API_KEY` are left alone.
func TestEveryConfigFieldIsDocumented(t *testing.T) {
	docs := readFieldDocs(t)
	leaves, sections := configPaths()
	for _, path := range slices.Sorted(maps.Keys(leaves)) {
		if _, ok := docs[path]; !ok {
			t.Errorf("%s is a setting in Config but has no heading in docs/config.md; add one with its **Type**, **Default** and what it does", path)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(docs)) {
		top, _, _ := strings.Cut(path, ".")
		_, isTopLeaf := leaves[top]
		if !sections[top] && !isTopLeaf {
			continue
		}
		if _, ok := leaves[path]; !ok && !sections[path] {
			t.Errorf("docs/config.md:%d: %s is documented but is not a setting in Config", docs[path].Line, path)
		}
	}
}

// The type a field is documented as must be the type it actually is.
//
// TestDocumentedExamplesActuallyLoad only validates whole YAML examples, so a
// field the examples happen not to exercise can be documented with the wrong
// type indefinitely. That is not hypothetical: graph.give_up was documented
// as a Boolean and graph.approval as a String, when give_up is an int cap and
// approval is a bool -- the two had been swapped, most likely by confusing
// config.Graph.GiveUp (how many fix attempts) with state.Fix.GiveUp (whether
// the loop gave up). A reader following either would have written a config
// the loader rejects.
//
// Every leaf must carry a **Type** line, at whatever heading level it sits.
// This once read only ### headings, so the eight settings under review.sonar
// and review.ai.mcp were never checked, and two of them were documented as
// durations when one is a bool and the other a count of seconds.
func TestDocumentedFieldTypesMatchTheStruct(t *testing.T) {
	docs := readFieldDocs(t)
	leaves, _ := configPaths()
	for _, path := range slices.Sorted(maps.Keys(leaves)) {
		doc, documented := docs[path]
		if !documented {
			continue // TestEveryConfigFieldIsDocumented reports it.
		}
		if doc.Type == "" {
			t.Errorf("docs/config.md:%d: %s has no **Type** line", doc.Line, path)
			continue
		}
		matches, recognised := documentedTypes[doc.Type]
		if !recognised {
			t.Errorf("docs/config.md:%d: %s: documented type %q is not one this test knows; add it to documentedTypes",
				doc.Line, path, doc.Type)
			continue
		}
		if !matches(leaves[path]) {
			t.Errorf("docs/config.md:%d: %s is documented as %q but is a %s in Config; a reader following the docs writes a config the loader rejects",
				doc.Line, path, doc.Type, leaves[path])
		}
	}
}

// Every setting's documented default must be the value defaults.go sets.
//
// A key left out of belay.yaml takes its default, so the **Default** line is
// the one a reader relies on most, and nothing else checked it. It had
// drifted on eleven settings: agent.max_turns was documented as 5 when it is
// 30, graph.max_steps as 20 when it is 60, graph.node_timeout as 10m when it
// is 15m, and budget.max_usd as 10 when it is 5, so a reader budgeting a run
// from the docs expected twice the ceiling belay enforces.
//
// The default is written as a YAML literal in the first pair of backticks. It
// is decoded by the real loader onto an empty Config and compared through
// Snapshot, the canonical text form a run manifest records, so `5.00` and
// the 5 defaults.go holds agree, as do "900s" and 15m, because each pair
// drives an identical run.
func TestDocumentedDefaultsMatchCode(t *testing.T) {
	docs := readFieldDocs(t)
	leaves, _ := configPaths()
	want := Default().Snapshot()
	for _, path := range slices.Sorted(maps.Keys(leaves)) {
		doc, documented := docs[path]
		if !documented {
			continue // TestEveryConfigFieldIsDocumented reports it.
		}
		literals := backticked(doc.Default)
		if len(literals) == 0 {
			t.Errorf("docs/config.md:%d: %s has no **Default** line with its value in backticks", doc.Line, path)
			continue
		}
		var got Config
		if err := got.Merge([]byte(yamlSetting(path, literals[0])), "docs/config.md"); err != nil {
			t.Errorf("docs/config.md:%d: %s: documented default `%s` does not load: %v", doc.Line, path, literals[0], err)
			continue
		}
		if got.Snapshot()[path] != want[path] {
			t.Errorf("docs/config.md:%d: %s is documented as defaulting to `%s`, but defaults.go sets %q",
				doc.Line, path, literals[0], want[path])
		}
	}
}

// A field's documented **Valid values** must be exactly the set the validator
// accepts, and only a field the validator checks as a closed set may carry
// such a line.
//
// Four lists had drifted, one of them entirely: agent.mode was documented as
// auto, streaming or batch, modes belay has never had, when it is live,
// record or replay. test.runner, review.fail_on and review.ai.mcp.mode each
// named values the loader rejects and missed values it accepts, so a reader
// who wrote a documented value had the config refused, and one who needed an
// undocumented value never learned it existed.
//
// "Valid values" promises that the loader rejects anything else. A setting
// whose names are checked only once a run uses them, such as agent.backend or
// review.sonar.runner, lists them as **Shipped values** instead, so the docs
// never promise a load-time check that does not happen. Order is not
// compared: the docs may group values the way their prose explains them.
func TestDocumentedEnumValuesMatchTheValidator(t *testing.T) {
	docs := readFieldDocs(t)
	closed := map[string]bool{}
	for _, field := range enumFields {
		closed[field.name] = true
		doc, documented := docs[field.name]
		if !documented {
			continue // TestEveryConfigFieldIsDocumented reports it.
		}
		var got []string
		for _, literal := range backticked(doc.Valid) {
			var value string
			if err := yaml.Unmarshal([]byte(literal), &value); err != nil {
				t.Errorf("docs/config.md:%d: %s: valid value `%s` is not a YAML string: %v", doc.Line, field.name, literal, err)
				continue
			}
			got = append(got, value)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(field.allowed))) {
			t.Errorf("docs/config.md:%d: %s documents valid values [%s], but the validator accepts [%s]",
				doc.Line, field.name, strings.Join(got, ", "), strings.Join(field.allowed, ", "))
		}
	}
	for _, path := range slices.Sorted(maps.Keys(docs)) {
		if doc := docs[path]; doc.Valid != "" && !closed[path] {
			t.Errorf("docs/config.md:%d: %s lists **Valid values**, but the loader accepts any value there; "+
				"call them **Shipped values**, or make the field a closed set in enumFields", doc.Line, path)
		}
	}
}

// backticked returns the text inside each pair of backticks in s, in order.
func backticked(s string) []string {
	var spans []string
	for {
		_, rest, ok := strings.Cut(s, "`")
		if !ok {
			return spans
		}
		span, after, ok := strings.Cut(rest, "`")
		if !ok {
			return spans
		}
		spans = append(spans, span)
		s = after
	}
}

// yamlSetting renders a document that sets the dotted path to literal, one
// nested mapping per segment.
func yamlSetting(path, literal string) string {
	var b strings.Builder
	segments := strings.Split(path, ".")
	for depth, segment := range segments {
		b.WriteString(strings.Repeat("  ", depth) + segment + ":")
		if depth == len(segments)-1 {
			b.WriteString(" " + literal)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// fieldDoc is what docs/config.md says about one field: the attribute lines
// written under a heading that names the field's dotted path in backticks.
// An attribute the heading does not carry is the empty string.
type fieldDoc struct {
	// Line is the 1-based line of the heading, so a failure names a place.
	Line int
	// Type is the text after "**Type**: ".
	Type string
	// Default is the text after "**Default**: ".
	Default string
	// Valid is the text after "**Valid values**: ".
	Valid string
}

// parseFieldDocs returns every heading in src that names a path in
// backticks, keyed by that path, with the attribute lines written under it.
//
// A heading at any level counts, because the docs nest deeper sections one
// level deeper: review.sonar.* sits under #### and review.ai.mcp.* under
// #####. An attribute line belongs to the nearest heading above it until the
// next heading of any level, and lines inside a fenced block are skipped, so
// a shell comment in an example is not read as a heading. The first of a
// repeated attribute wins.
func parseFieldDocs(src string) map[string]fieldDoc {
	docs := map[string]fieldDoc{}
	current, inFence := "", false
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if path, ok := headingPath(line); ok {
			current = path
			if path != "" {
				docs[path] = fieldDoc{Line: i + 1}
			}
			continue
		}
		if current == "" {
			continue
		}
		doc := docs[current]
		for _, attr := range []struct {
			label string
			field *string
		}{
			{"**Type**: ", &doc.Type},
			{"**Default**: ", &doc.Default},
			{"**Valid values**: ", &doc.Valid},
		} {
			if value, ok := strings.CutPrefix(line, attr.label); ok && *attr.field == "" {
				*attr.field = strings.TrimSpace(value)
			}
		}
		docs[current] = doc
	}
	return docs
}

// headingPath reports whether line is a markdown heading and, if the heading
// opens with a backticked path, that path. A trailing qualifier after the
// closing backtick is ignored.
func headingPath(line string) (string, bool) {
	text := strings.TrimLeft(line, "#")
	if text == line || !strings.HasPrefix(text, " ") {
		return "", false
	}
	quoted, ok := strings.CutPrefix(strings.TrimSpace(text), "`")
	if !ok {
		return "", true
	}
	path, _, _ := strings.Cut(quoted, "`")
	return path, true
}

func TestParseFieldDocs(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want map[string]fieldDoc
	}{
		{
			name: "every attribute, markdown line breaks trimmed",
			src:  "## `version`\n\n**Type**: Integer  \n**Default**: `1`  \n**Valid values**: `1`\n",
			want: map[string]fieldDoc{"version": {Line: 1, Type: "Integer", Default: "`1`", Valid: "`1`"}},
		},
		{
			name: "any heading level, with a qualifier after the path",
			src:  "# Title\n\n##### `review.ai.mcp.mode` (if `review.mode == \"ai\"`)\n**Type**: String\n",
			want: map[string]fieldDoc{"review.ai.mcp.mode": {Line: 3, Type: "String"}},
		},
		{
			name: "a heading without a path ends the field above it",
			src:  "### `graph.give_up`\n**Type**: Integer\n\n## Environment Variables\n**Type**: String\n",
			want: map[string]fieldDoc{"graph.give_up": {Line: 1, Type: "Integer"}},
		},
		{
			name: "a comment inside a fenced block is not a heading",
			src:  "### `budget.max_usd`\n```bash\n# belay.yaml says max_usd: 10.0\n```\n**Default**: `5.00`\n",
			want: map[string]fieldDoc{"budget.max_usd": {Line: 1, Default: "`5.00`"}},
		},
		{
			name: "an insertion marker between fields changes nothing",
			src:  "### `fanout.select`\n**Type**: String\n\n<!-- slice:output -->\n\n## `output`\n\n### `output.mode`\n**Type**: String\n",
			want: map[string]fieldDoc{
				"fanout.select": {Line: 1, Type: "String"},
				"output":        {Line: 6},
				"output.mode":   {Line: 8, Type: "String"},
			},
		},
		{
			name: "the first of a repeated attribute wins",
			src:  "### `agent.mode`\n**Default**: `\"live\"`\n**Default**: `\"auto\"`\n",
			want: map[string]fieldDoc{"agent.mode": {Line: 1, Default: "`\"live\"`"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, parseFieldDocs(tt.src)); diff != "" {
				t.Errorf("parseFieldDocs (-want +got):\n%s", diff)
			}
		})
	}
}
