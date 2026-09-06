//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// Durability suite: this file and durability_test.go are the end-to-end
// proof of belay's headline claim -- an agent run survives a crash and
// resumes from the last completed node.
//
// Where loopHarness (loop_harness.go) drives the real internal/nodes.Default
// registry to prove the graph's wiring fits together, this file drives
// small, fully controlled Node implementations of its own. Crash/resume
// correctness is a property of internal/graph.Dispatcher, internal/journal
// and internal/state alone -- per ADR 0002, a node is pure and contributes
// nothing to it -- so testing against a minimal, deterministic graph
// isolates the property this suite exists to prove from the unrelated
// complexity of a real coding agent, test runner or linter. That split
// mirrors this repository's own testing guidance: test at the lowest level
// that still captures the behavior in question.
//
// Every package-level identifier in this file (and in durability_test.go) is
// prefixed "durability" so it cannot collide with loopHarness's own
// identifiers in this same package.
const durabilityRunID = "20260101T000000Z-d17ab111ad00"

// durabilityHarness owns one run directory across a durability test's
// lifetime: manifest.json, state.json and journal.ndjson under
// <workspace>/.belay/runs/<run id>.
//
// It deliberately holds no *graph.Dispatcher, no budget.Ledger and no open
// *journal.Journal between calls -- those are exactly the in-memory objects
// a real crash destroys. (*durabilityHarness).dispatcher builds a brand-new
// Dispatcher, over a brand-new Store, every time it is called; a test
// "crashes" by abandoning the previous one (see durabilityRunExpectingCrash)
// and asking the harness for a new one, exactly as a fresh `belay run`
// process would attach to a run directory it did not just finish writing.
type durabilityHarness struct {
	t         *testing.T
	workspace string
	layout    state.Layout
	cfg       config.Config
}

// newDurabilityRun creates a fresh run directory with a manifest and initial
// state.json, exactly as `belay run` does before starting its graph.
func newDurabilityRun(t *testing.T, cfg config.Config, goal string) *durabilityHarness {
	t.Helper()

	ws := t.TempDir()
	layout, err := state.NewLayout(ws, durabilityRunID)
	if err != nil {
		t.Fatalf("state.NewLayout() error = %v", err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", layout.RunDir(), err)
	}

	store := state.NewStore(layout)
	man := state.NewManifest(time.Now().UTC(), durabilityRunID, ws, goal, cfg)
	if err := store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}
	if err := store.SaveState(state.NewState(goal)); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	return &durabilityHarness{t: t, workspace: ws, layout: layout, cfg: cfg}
}

// dispatcher builds a brand-new *graph.Dispatcher, over a brand-new
// *state.Store, against h's run directory. See the type doc: never reuse the
// value this returns across a simulated crash.
func (h *durabilityHarness) dispatcher(nodes ...graph.Node) *graph.Dispatcher {
	h.t.Helper()
	reg := graph.NewRegistry()
	reg.MustRegister(nodes...)
	d, err := graph.NewDispatcher(graph.Options{
		Store: state.NewStore(h.layout), Registry: reg, Config: h.cfg,
	})
	if err != nil {
		h.t.Fatalf("graph.NewDispatcher() error = %v", err)
	}
	return d
}

// seedJournal appends recs through the real journal.Append, standing in for
// whatever a previous process durably wrote before this test takes over: the
// bytes on disk are exactly what belay's own journal writer produces, never
// a hand-built file.
func (h *durabilityHarness) seedJournal(recs ...journal.Record) {
	h.t.Helper()
	j, err := journal.Open(h.layout.JournalPath())
	if err != nil {
		h.t.Fatalf("journal.Open() error = %v", err)
	}
	for _, r := range recs {
		if _, err := j.Append(r); err != nil {
			h.t.Fatalf("journal.Append(%v) error = %v", r.Event, err)
		}
	}
	if err := j.Close(); err != nil {
		h.t.Fatalf("journal.Close() error = %v", err)
	}
}

// readJournal streams the journal exactly as journal.ReadFile reports it,
// torn tail and all. Unlike timeline, it never fails the test, so a scenario
// that deliberately tears the file can inspect the result first.
func (h *durabilityHarness) readJournal() journal.ReadResult {
	h.t.Helper()
	res, err := journal.ReadFile(h.layout.JournalPath())
	if err != nil {
		h.t.Fatalf("journal.ReadFile() error = %v", err)
	}
	return res
}

// timeline renders the journal's records compactly for assertion, first
// failing the test outright if they do not form a structurally valid
// history: every terminal path through the dispatcher must leave a journal
// the next process can open and trust.
func (h *durabilityHarness) timeline() []string {
	h.t.Helper()
	res := h.readJournal()
	if res.Incomplete {
		h.t.Fatalf("journal has a torn tail where a clean shutdown was expected")
	}
	if err := journal.Validate(res.Records); err != nil {
		h.t.Fatalf("journal.Validate() = %v, want nil", err)
	}

	out := make([]string, 0, len(res.Records))
	for _, r := range res.Records {
		switch r.Event {
		case journal.EventNodeStarted:
			out = append(out, fmt.Sprintf("%s#%d started", r.Node, r.Attempt))
		case journal.EventNodeFinished:
			s := fmt.Sprintf("%s#%d finished %s", r.Node, r.Attempt, r.Status)
			if r.Next != "" {
				s += " -> " + r.Next
			}
			out = append(out, s)
		default:
			out = append(out, r.Event.String())
		}
	}
	return out
}

// manifest reads the run's current manifest.json directly off disk.
func (h *durabilityHarness) manifest() state.Manifest {
	h.t.Helper()
	m, err := state.LoadManifest(h.layout.ManifestPath())
	if err != nil {
		h.t.Fatalf("LoadManifest() error = %v", err)
	}
	return m
}

// state reads the run's current state.json directly off disk.
func (h *durabilityHarness) state() state.State {
	h.t.Helper()
	st, err := state.LoadState(h.layout.StatePath())
	if err != nil {
		h.t.Fatalf("LoadState() error = %v", err)
	}
	return st
}

// saveState overwrites state.json directly, standing in for the exact bytes
// a crashed process's own (already-applied) Store.ApplyPatch left behind.
// See TestDurability_CrashBetweenPatchAndJournal_IsNotDoubleApplied for why
// this one scenario cannot be produced by running the real dispatcher: the
// window it reconstructs is inside an unexported method, one write wide,
// with no exported hook landing inside it.
func (h *durabilityHarness) saveState(st state.State) {
	h.t.Helper()
	if err := state.SaveState(h.layout.StatePath(), st); err != nil {
		h.t.Fatalf("SaveState() error = %v", err)
	}
}

// durabilityEqualStrings reports whether got and want hold the same strings
// in the same order.
func durabilityEqualStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// durabilityCloseTo reports whether got and want agree to within float64
// rounding, for a dollar figure that has passed through the budget ledger's
// micro-dollar conversion (see internal/budget's "Float discipline").
func durabilityCloseTo(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

// ---------------------------------------------------------------------------
// A minimal, fully controlled node graph.
// ---------------------------------------------------------------------------

// durabilityStep scripts one call of a durabilityNode.
type durabilityStep struct {
	res graph.Result
	// crash simulates the process dying inside this call: Run panics with a
	// durabilityCrash instead of returning, and must be paired with
	// durabilityRunExpectingCrash at the call site.
	crash bool
}

// durabilityOK returns a durabilityNode with one scripted step: succeed and
// route to next.
func durabilityOK(name, next string) *durabilityNode {
	return &durabilityNode{name: name, steps: []durabilityStep{
		{res: graph.Result{Next: next, Status: journal.StatusOK}},
	}}
}

// durabilityNode replays a script of durabilitySteps in call order and
// records what it observed on each call: how many times it ran, at which
// attempt, and what State it was handed. When the script is shorter than the
// number of calls, the last step repeats.
type durabilityNode struct {
	name  string
	steps []durabilityStep

	calls    int
	attempts []int
	seenFix  []state.Fix
	seenCode []state.Code
}

func (n *durabilityNode) Name() string { return n.name }

func (n *durabilityNode) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	n.calls++
	n.attempts = append(n.attempts, rc.Attempt)
	n.seenFix = append(n.seenFix, rc.State.Fix)
	n.seenCode = append(n.seenCode, rc.State.Code)

	idx := n.calls - 1
	if idx >= len(n.steps) {
		idx = len(n.steps) - 1
	}
	st := n.steps[idx]
	if st.crash {
		panic(durabilityCrash{node: n.name, attempt: rc.Attempt})
	}
	return st.res, nil
}

// durabilityCrash is the value a durabilityNode panics with to simulate the
// process dying partway through a node's execution. An unhandled panic, a
// segfault and a SIGKILL are indistinguishable from the dispatcher's point
// of view: none of them ever lets node.Run return, which is the only thing
// that matters for what ends up durable on disk.
type durabilityCrash struct {
	node    string
	attempt int
}

// durabilityRunExpectingCrash calls run and requires it to end in a panic
// carrying a durabilityCrash, then swallows it -- standing in for an
// operator's "the process died" swallowing whatever it died doing. Nothing
// after the injected panic in the dispatcher's call stack ever executes, in
// particular settle() and every write it would have made.
//
// A panic that is not a durabilityCrash is re-raised: that would be a real
// bug surfacing through this harness, not the crash the test asked for, and
// it must fail loudly rather than be mistaken for the injected one.
func durabilityRunExpectingCrash(t *testing.T, run func() (graph.Outcome, error)) durabilityCrash {
	t.Helper()
	var crash durabilityCrash
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			c, ok := r.(durabilityCrash)
			if !ok {
				panic(r)
			}
			crash = c
		}()
		out, err := run()
		t.Fatalf("expected a simulated crash, but the run returned normally: outcome=%+v err=%v", out, err)
	}()
	if crash.node == "" {
		t.Fatal("expected a simulated crash, got none")
	}
	return crash
}

// ---------------------------------------------------------------------------
// The ADR 0002 guard: nothing but the dispatcher writes control state.
// ---------------------------------------------------------------------------

// durabilityControlSnapshot is a content fingerprint of the three files ADR
// 0002 reserves for the dispatcher: state.json, manifest.json and
// journal.ndjson. Hashing content, rather than stat'ing size and mtime,
// means even a same-size overwrite landing within one filesystem's mtime
// resolution is still detected.
type durabilityControlSnapshot struct {
	state, manifest, journal string
}

// durabilityFingerprint hashes the three control files at layout's run
// directory. A file that does not exist yet (journal.ndjson, before its
// first record) fingerprints as the sentinel "<absent>" rather than erroring.
func durabilityFingerprint(layout state.Layout) (durabilityControlSnapshot, error) {
	st, err := durabilityHashFile(layout.StatePath())
	if err != nil {
		return durabilityControlSnapshot{}, err
	}
	man, err := durabilityHashFile(layout.ManifestPath())
	if err != nil {
		return durabilityControlSnapshot{}, err
	}
	jrn, err := durabilityHashFile(layout.JournalPath())
	if err != nil {
		return durabilityControlSnapshot{}, err
	}
	return durabilityControlSnapshot{state: st, manifest: man, journal: jrn}, nil
}

func durabilityHashFile(path string) (string, error) {
	// #nosec G304 -- path always comes from state.Layout, confined to a
	// t.TempDir() run directory this process created for itself.
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>", nil
	}
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// durabilityTB is the sliver of testing.TB a durabilityGuardedNode needs.
// *testing.T satisfies it; so does durabilityFakeT, which lets one test
// prove the guard below actually fires on a violation without that failure
// also failing the real *testing.T hosting the proof.
type durabilityTB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// durabilityFakeT records Errorf/Fatalf calls instead of acting on them, so
// a test can drive a deliberately hostile node through a real Dispatcher and
// then assert the guard caught the violation.
type durabilityFakeT struct {
	errors []string
}

func (f *durabilityFakeT) Helper() {}

func (f *durabilityFakeT) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func (f *durabilityFakeT) Fatalf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
	panic(f)
}

// durabilityGuardedNode wraps a legitimate Result behind a check that
// state.json, manifest.json and journal.ndjson did not change between the
// moment this node's Run starts and the moment it returns.
//
// This is a sound check, not merely a plausible one: the dispatcher's own
// loop (internal/graph/dispatcher.go) never touches any of the three between
// calling node.Run and settle()ing its Result, so the only code that could
// move them during that window is the node's own Run method.
type durabilityGuardedNode struct {
	tb     durabilityTB
	name   string
	layout state.Layout
	work   func(rc *graph.RunContext) graph.Result
}

func (n *durabilityGuardedNode) Name() string { return n.name }

func (n *durabilityGuardedNode) Run(_ context.Context, rc *graph.RunContext) (graph.Result, error) {
	n.tb.Helper()
	before, err := durabilityFingerprint(n.layout)
	if err != nil {
		n.tb.Fatalf("%s: fingerprint control state on entry: %v", n.name, err)
	}
	res := n.work(rc)
	after, err := durabilityFingerprint(n.layout)
	if err != nil {
		n.tb.Fatalf("%s: fingerprint control state on exit: %v", n.name, err)
	}
	if before != after {
		n.tb.Errorf("%s: state.json/manifest.json/journal.ndjson changed during this node's own "+
			"Run() call; ADR 0002 reserves them for the dispatcher alone (before=%+v after=%+v)",
			n.name, before, after)
	}
	return res, nil
}
