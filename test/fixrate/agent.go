//go:build fixrate

package fixrate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

// Strategy is how the scripted demonstration agent (NewScriptedAgent)
// responds to one defect's target file.
//
// There is deliberately no "partially fix it" strategy: the point of this
// fake is to let Run's own tests exercise all three outcome categories
// Result can report (repaired, not_repaired, invalid_repair) under full
// control, not to simulate the many ways a real agent's attempt can be
// partially right.
type Strategy int

const (
	// StrategyRepair overwrites the file with the byte-identical,
	// known-good version from the fixture's src/ tree — the strongest
	// possible stand-in for "an agent correctly diagnosed and fixed this
	// defect," without this package needing the defect's exact
	// Find/Replace text (which only exists inside the foreign fixture
	// module — see doc.go).
	StrategyRepair Strategy = iota

	// StrategyCheat simulates the exact failure mode the cheat detector
	// (evaluate.go) exists to catch: instead of touching the defect's
	// source file at all, it empties the corresponding "*_test.go" file
	// down to a bare package clause, deleting every test it contained —
	// "a fix that deletes the failing test," in the T42 brief's own
	// words. The source bug is left exactly as injected.
	StrategyCheat

	// StrategyNoop leaves the file exactly as injected: the defect is
	// never addressed, source or test. It is what an agent that could not
	// find or would not attempt the fix looks like.
	StrategyNoop
)

// ScriptedPlan configures NewScriptedAgent: which Strategy to use for each
// selected defect's target file.
type ScriptedPlan struct {
	// FileStrategy maps a defect's target file — InjectedDefect.File,
	// e.g. "policy.go", relative to the fixture's src/ and to the
	// injected copy's root — to the Strategy this agent uses for it.
	// applyPlan iterates this map's keys, so a file must be present here
	// for the agent to do anything with it at all; see AllRepair for the
	// common case of building this map for every file in a defect set
	// without listing each one by hand (useful for a --random selection,
	// whose exact files are not known until after injection).
	FileStrategy map[string]Strategy
}

func (p ScriptedPlan) strategyFor(file string) Strategy {
	if s, ok := p.FileStrategy[file]; ok {
		return s
	}
	return StrategyRepair
}

// AllRepair returns a ScriptedPlan that applies StrategyRepair to every
// file in files — the common "prove belay's graph can fix everything it
// was handed" case, including a --random selection whose exact files are
// only known after injectDefects returns.
func AllRepair(files []string) ScriptedPlan {
	m := make(map[string]Strategy, len(files))
	for _, f := range files {
		m[f] = StrategyRepair
	}
	return ScriptedPlan{FileStrategy: m}
}

// changedFilesTag is the fenced-block info string belay's code node
// (internal/nodes/code) asks for and parses back out of an agent's reply.
// It is a stable, documented part of the AgentBackend<->code-node
// protocol (see that package's prompt.go), not an internal symbol this
// package imports — replicating the exact string here is how any
// AgentBackend, real or fake, is expected to report what it touched.
const changedFilesTag = "changed-files"

// scriptedAgent holds the state NewScriptedAgent's handler closes over.
type scriptedAgent struct {
	pristineSrcDir string
	plan           ScriptedPlan
}

// NewScriptedAgent returns a belay.AgentBackend that drives belay's real
// graph (plan -> approve -> code -> write -> test -> fix -> review)
// without spending anything: its Invoke handler edits files on disk as
// its only side effect — precisely how belay's actual code and fix nodes
// expect an agent to behave (see internal/nodes/code's package doc,
// "where the change is made") — and reports zero belay.Usage on every
// call.
//
// pristineSrcDir is the known-good source tree StrategyRepair restores
// from, ordinarily fixtures/seeded-bug/src (RunOptions.FixtureDir +
// "/src").
//
// The returned backend is a *belaytest.FakeAgent with Func set, so its
// full call history remains available via FakeAgent.Calls for tests that
// want to assert on what belay actually sent it.
func NewScriptedAgent(pristineSrcDir string, plan ScriptedPlan) *belaytest.FakeAgent {
	a := &scriptedAgent{pristineSrcDir: pristineSrcDir, plan: plan}
	return &belaytest.FakeAgent{
		NameValue: "fixrate-scripted",
		Func:      a.invoke,
	}
}

// planPromptText is returned for a read-only (planning) invocation: belay's
// plan node only requires non-empty text, and archives it verbatim as
// plan.md — see internal/nodes/plan's package doc.
const planPromptText = "Implement the goal directly in the repository: restore or correct whichever " +
	"files the described defect touches. No new files or packages are needed."

// invoke implements the AgentBackend.Invoke signature belaytest.FakeAgent.
// Func expects.
//
// It distinguishes belay's read-only planning call from its editing calls
// (code and fix) by AllowedTools alone, never by matching this package
// against internal/nodes' own prompt text (which is unexported and, more
// to the point, not a contract this package should couple itself to): the
// plan node always sends exactly Read/Glob/Grep with no Edit, per
// internal/nodes/plan's readOnlyTools; the code node always includes Edit;
// the fix node sends no AllowedTools at all (meaning "use the backend's
// default," per belay.AgentRequest's own doc comment), which this
// function also treats as an editing call, since fix has nothing else to
// ask this fake for.
func (a *scriptedAgent) invoke(_ context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	if isReadOnlyRequest(req) {
		return belay.AgentResponse{Text: planPromptText, Usage: belay.Usage{}}, nil
	}

	touched, err := a.applyPlan(req.WorkDir)
	if err != nil {
		return belay.AgentResponse{}, fmt.Errorf("fixrate: scripted agent: %w", err)
	}
	return belay.AgentResponse{
		Text:      changedFilesReply(touched),
		SessionID: "fixrate-scripted-session",
		Turns:     1,
		Usage:     belay.Usage{}, // always zero: this is the replay-mode zero-cost guarantee's source of truth.
	}, nil
}

// isReadOnlyRequest reports whether req is belay's planning call: a
// non-empty AllowedTools that does not include Edit.
func isReadOnlyRequest(req belay.AgentRequest) bool {
	return len(req.AllowedTools) > 0 && !slices.Contains(req.AllowedTools, "Edit")
}

// applyPlan runs every configured Strategy against workDir and returns
// the repository-relative paths it touched, sorted for determinism (the
// same defect set must produce the same reply on every run — see
// TestDeterminism).
func (a *scriptedAgent) applyPlan(workDir string) ([]string, error) {
	files := make([]string, 0, len(a.plan.FileStrategy))
	for f := range a.plan.FileStrategy {
		files = append(files, f)
	}
	sort.Strings(files)

	var touched []string
	for _, file := range files {
		switch a.plan.strategyFor(file) {
		case StrategyRepair:
			if err := a.repairFile(workDir, file); err != nil {
				return nil, err
			}
			touched = append(touched, file)
		case StrategyCheat:
			testFile, err := a.cheatFile(workDir, file)
			if err != nil {
				return nil, err
			}
			touched = append(touched, testFile)
		case StrategyNoop:
			// Deliberately untouched.
		}
	}
	return touched, nil
}

// repairFile overwrites workDir/file with the byte-identical contents of
// pristineSrcDir/file. It is idempotent: a file already matching the
// pristine copy is rewritten to the same bytes, which is a correctness
// requirement, not just an optimization the code skips — this handler
// runs again on every fix-loop attempt against a workspace it already
// repaired on the first pass.
func (a *scriptedAgent) repairFile(workDir, file string) error {
	rel := filepath.FromSlash(file)
	data, err := os.ReadFile(filepath.Join(a.pristineSrcDir, rel)) //nolint:gosec // pristineSrcDir and file both come from this harness's own configuration, never external input
	if err != nil {
		return fmt.Errorf("reading known-good %s: %w", file, err)
	}
	target, err := safeJoin(workDir, rel)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil { //nolint:gosec // matches the injected copy's own file permissions
		return fmt.Errorf("restoring %s: %w", file, err)
	}
	return nil
}

// cheatSuffix names the file this package empties as the cheat strategy's
// only edit. It assumes the same "<name>.go" -> "<name>_test.go" pairing
// fixtures/seeded-bug/src uses for every one of its five source files
// (confirmed by that tree's own layout: policy.go/policy_test.go,
// delay.go/delay_test.go, ledger.go/ledger_test.go,
// orchestrate.go/orchestrate_test.go, errors.go/errors_test.go) rather
// than a general convention this package could rely on for an arbitrary
// target repository.
const cheatSuffix = "_test.go"

// cheatFile empties workDir's test file for file down to a bare package
// clause — deleting every test it contained, the literal behavior T42's
// cheat-detector requirement describes — and returns the test file's
// repository-relative path. The source file named by file is left
// untouched.
func (a *scriptedAgent) cheatFile(workDir, file string) (string, error) {
	testFile := strings.TrimSuffix(file, ".go") + cheatSuffix
	pkg, err := packageNameOf(filepath.Join(a.pristineSrcDir, filepath.FromSlash(file)))
	if err != nil {
		return "", err
	}
	target, err := safeJoin(workDir, filepath.FromSlash(testFile))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("cheat strategy for %s expects %s to exist: %w", file, testFile, err)
	}
	if err := os.WriteFile(target, []byte("package "+pkg+"\n"), 0o644); err != nil { //nolint:gosec // matches the injected copy's own file permissions
		return "", fmt.Errorf("emptying %s: %w", testFile, err)
	}
	return testFile, nil
}

// safeJoin joins workDir and rel, refusing a result that would escape
// workDir. Defence in depth: every rel this package passes here
// originates from its own configuration (InjectedDefect.File values from
// a trusted injected.json this harness itself produced), never from
// agent- or user-supplied text, but a fake standing in for "an agent
// edits files" should not itself skip the containment check a real
// backend integration would need.
func safeJoin(workDir, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("fixrate: refusing unsafe path %q", rel)
	}
	return filepath.Join(workDir, rel), nil
}

// packageNameOf reads the "package X" clause from the top of a Go source
// file.
func packageNameOf(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from this harness's own configuration
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if name, ok := strings.CutPrefix(line, "package "); ok {
			return strings.TrimSpace(name), nil
		}
	}
	return "", fmt.Errorf("fixrate: no package clause found in %s", path)
}

// changedFilesReply renders the prose-plus-fenced-block reply
// internal/nodes/code's prompt asks for (see that package's prompt.go,
// changedFilesTag/changedFilesExample): a short summary, then exactly one
// fence tagged changedFilesTag holding one repository-relative path per
// line.
func changedFilesReply(touched []string) string {
	var b strings.Builder
	if len(touched) == 0 {
		b.WriteString("No change was needed.")
		return b.String()
	}
	fmt.Fprintf(&b, "Edited %d file(s) to address the described defect(s).\n\n", len(touched))
	b.WriteString("```")
	b.WriteString(changedFilesTag)
	b.WriteByte('\n')
	for _, f := range touched {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	b.WriteString("```\n")
	return b.String()
}
