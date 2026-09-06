package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/google/go-cmp/cmp"
)

// timelineBase is the fixture clock. Every fixture time is derived from it,
// so every expected duration below is hand-computable.
var timelineBase = time.Date(2026, 9, 5, 14, 25, 30, 0, time.UTC)

// timelineNodeSpec describes one node execution to synthesize.
type timelineNodeSpec struct {
	node       string
	attempt    int
	status     journal.Status
	durationMS int64
	next       string
	usage      *journal.Usage
	note       string
}

// timelineBuilder assembles a valid journal history one record at a time,
// assigning seq and advancing a deterministic clock exactly as a real run
// would: one second of dispatcher overhead before each node, then the node's
// own recorded duration.
type timelineBuilder struct {
	records []journal.Record
	seq     uint64
	clock   time.Time
}

func newTimelineBuilder() *timelineBuilder {
	return &timelineBuilder{clock: timelineBase}
}

func (b *timelineBuilder) add(rec journal.Record) {
	b.seq++
	rec.Seq = b.seq
	rec.Time = b.clock
	b.records = append(b.records, rec)
}

// run appends a run-level lifecycle record after advancing the clock by one
// second.
func (b *timelineBuilder) run(ev journal.Event) *timelineBuilder {
	b.clock = b.clock.Add(time.Second)
	b.add(journal.Record{Event: ev})
	return b
}

// pauseFor appends run_paused, then run_resumed d later, modelling a human
// sitting at the approval gate.
func (b *timelineBuilder) pauseFor(d time.Duration) *timelineBuilder {
	b.clock = b.clock.Add(time.Second)
	b.add(journal.Record{Event: journal.EventRunPaused})
	b.clock = b.clock.Add(d)
	b.add(journal.Record{Event: journal.EventRunResumed})
	return b
}

// node appends a matched node_started / node_finished pair.
func (b *timelineBuilder) node(s timelineNodeSpec) *timelineBuilder {
	attempt := s.attempt
	if attempt == 0 {
		attempt = 1
	}
	b.clock = b.clock.Add(time.Second)
	b.add(journal.Record{Node: s.node, Attempt: attempt, Event: journal.EventNodeStarted})
	b.clock = b.clock.Add(time.Duration(s.durationMS) * time.Millisecond)
	b.add(journal.Record{
		Node:       s.node,
		Attempt:    attempt,
		Event:      journal.EventNodeFinished,
		DurationMS: s.durationMS,
		Status:     s.status,
		Next:       s.next,
		Usage:      s.usage,
		Note:       s.note,
	})
	return b
}

// openNode appends a node_started with no matching node_finished: the shape a
// journal has when the process was killed while that node was running.
func (b *timelineBuilder) openNode(node string, attempt int) *timelineBuilder {
	b.clock = b.clock.Add(time.Second)
	b.add(journal.Record{Node: node, Attempt: attempt, Event: journal.EventNodeStarted})
	return b
}

func usd(in, out int64, amount float64, estimated bool) *journal.Usage {
	return &journal.Usage{InputTokens: in, OutputTokens: out, USD: amount, Estimated: estimated}
}

// timelineFixLoopRun is the canonical fixture: a run that fails its tests
// twice and repairs twice before passing.
//
//	run_started
//	plan -> approve -> code -> write
//	test(failed) -> fix -> test(failed) -> fix -> test(ok) -> review
//	run_completed
//
// Hand-computed expectations, used by the totals test:
//
//	agent time = 12400+200+108000+900+33100+41700+31900+38200+30500+22800
//	           = 319700ms
//	wall time  = agent time + 11 one-second gaps       = 330700ms
//	spend      = .031+.412+.004+.180+.004+.201+.004+.092 = $0.928
//	tokens in  = 4200+18500+900+12000+900+13100+900+7400 = 57900
//	tokens out = 1800+9200+210+5400+210+5900+210+2600     = 25530
func timelineFixLoopRun() []journal.Record {
	return newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 12400, next: "approve",
			usage: usd(4200, 1800, 0.031, false)}).
		node(timelineNodeSpec{node: "approve", status: journal.StatusOK, durationMS: 200, next: "code"}).
		node(timelineNodeSpec{node: "code", status: journal.StatusOK, durationMS: 108000, next: "write",
			usage: usd(18500, 9200, 0.412, false)}).
		node(timelineNodeSpec{node: "write", status: journal.StatusOK, durationMS: 900, next: "test"}).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 33100, next: "fix",
			usage: usd(900, 210, 0.004, false), note: "3 of 41 tests failing in pagination_test.go"}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 41700, next: "test",
			usage: usd(12000, 5400, 0.180, true)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 31900, next: "fix",
			usage: usd(900, 210, 0.004, false), note: "1 of 41 tests failing in pagination_test.go"}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 38200, next: "test",
			usage: usd(13100, 5900, 0.201, false)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusOK, durationMS: 30500, next: "review",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "review", status: journal.StatusOK, durationMS: 22800,
			usage: usd(7400, 2600, 0.092, false)}).
		run(journal.EventRunCompleted).
		records
}

func timelineResult(records []journal.Record, incomplete bool) journal.ReadResult {
	return journal.ReadResult{Records: records, Incomplete: incomplete}
}

func TestTimelineBuildOrdersAndNumbersLoopIterations(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(TimelineSelection{RunID: "r1"}, nil, timelineResult(timelineFixLoopRun(), false))

	type row struct {
		index      int
		node       string
		iteration  int
		iterations int
		outcome    TimelineOutcome
	}
	got := make([]row, 0, len(tl.Entries))
	for _, e := range tl.Entries {
		got = append(got, row{e.Index, e.Node, e.Iteration, e.Iterations, e.Outcome})
	}
	want := []row{
		{1, "plan", 1, 1, TimelineFinished},
		{2, "approve", 1, 1, TimelineFinished},
		{3, "code", 1, 1, TimelineFinished},
		{4, "write", 1, 1, TimelineFinished},
		{5, "test", 1, 3, TimelineFailed},
		{6, "fix", 1, 2, TimelineFinished},
		{7, "test", 2, 3, TimelineFailed},
		{8, "fix", 2, 2, TimelineFinished},
		{9, "test", 3, 3, TimelineFinished},
		{10, "review", 1, 1, TimelineFinished},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(row{})); diff != "" {
		t.Errorf("entries mismatch (-want +got):\n%s", diff)
	}

	wantLoops := []TimelineLoop{{Node: "test", Executions: 3}, {Node: "fix", Executions: 2}}
	if diff := cmp.Diff(wantLoops, tl.Loops); diff != "" {
		t.Errorf("loops mismatch (-want +got):\n%s", diff)
	}
	if tl.Status != "finished" {
		t.Errorf("Status = %q, want %q", tl.Status, "finished")
	}
	if tl.Interrupted {
		t.Error("Interrupted = true, want false for a completed run")
	}
}

// TestTimelineIterationCountsAgreeWithJournalStats pins the contract that the
// per-entry iteration count is the same number journal.Stats derives, so the
// two never disagree in the rendered output.
func TestTimelineIterationCountsAgreeWithJournalStats(t *testing.T) {
	t.Parallel()

	records := timelineFixLoopRun()
	tl := BuildTimeline(TimelineSelection{}, nil, timelineResult(records, false))

	fromStats := make(map[string]int)
	for _, s := range journal.Stats(records) {
		fromStats[s.Node] = s.Executions
	}
	for _, e := range tl.Entries {
		if got, want := e.Iterations, fromStats[e.Node]; got != want {
			t.Errorf("entry %d (%s): Iterations = %d, journal.Stats says %d", e.Index, e.Node, got, want)
		}
	}
}

func TestTimelineTotalsMatchHandComputedValues(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(TimelineSelection{}, nil, timelineResult(timelineFixLoopRun(), false))

	if got, want := tl.Totals.Nodes, 10; got != want {
		t.Errorf("Nodes = %d, want %d", got, want)
	}
	if got, want := tl.Totals.AgentDuration, 319700*time.Millisecond; got != want {
		t.Errorf("AgentDuration = %v, want %v", got, want)
	}
	if got, want := tl.Totals.WallDuration, 330700*time.Millisecond; got != want {
		t.Errorf("WallDuration = %v, want %v", got, want)
	}
	if got, want := tl.Totals.USD, 0.928; math.Abs(got-want) > 1e-9 {
		t.Errorf("USD = %v, want %v", got, want)
	}
	if got, want := tl.Totals.TokensIn, int64(57900); got != want {
		t.Errorf("TokensIn = %d, want %d", got, want)
	}
	if got, want := tl.Totals.TokensOut, int64(25530); got != want {
		t.Errorf("TokensOut = %d, want %d", got, want)
	}
	if !tl.Totals.Estimated {
		t.Error("Estimated = false, want true: one fix node reported an estimated cost")
	}
	// Wall clock must exceed agent time: the run spent real seconds between
	// nodes that no node's own duration accounts for.
	if tl.Totals.WallDuration <= tl.Totals.AgentDuration {
		t.Errorf("WallDuration %v should exceed AgentDuration %v", tl.Totals.WallDuration, tl.Totals.AgentDuration)
	}
}

func TestTimelineBuildInterruptedMidNode(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 1000, next: "code"}).
		openNode("code", 1).
		records

	tl := BuildTimeline(TimelineSelection{}, nil, timelineResult(records, true))

	if len(tl.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(tl.Entries))
	}
	last := tl.Entries[1]
	if last.Node != "code" || last.Outcome != TimelineInterrupted {
		t.Errorf("last entry = %s/%s, want code/interrupted", last.Node, last.Outcome)
	}
	if last.Duration != 0 || !last.FinishedAt.IsZero() {
		t.Errorf("interrupted entry should carry no duration or end time, got %v / %v", last.Duration, last.FinishedAt)
	}
	if !tl.Interrupted || tl.InterruptedNode != "code" {
		t.Errorf("Interrupted = %v, InterruptedNode = %q, want true/code", tl.Interrupted, tl.InterruptedNode)
	}
	if tl.Status != "interrupted" {
		t.Errorf("Status = %q, want %q", tl.Status, "interrupted")
	}
	// An interrupted node is not billed, so it must not inflate agent time.
	if got, want := tl.Totals.AgentDuration, time.Second; got != want {
		t.Errorf("AgentDuration = %v, want %v", got, want)
	}
}

func TestTimelineBuildEmptyJournal(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(TimelineSelection{RunID: "r1"}, nil, journal.ReadResult{})

	if len(tl.Entries) != 0 {
		t.Errorf("len(Entries) = %d, want 0", len(tl.Entries))
	}
	if tl.Interrupted {
		t.Error("Interrupted = true, want false for an empty journal")
	}
	if tl.Totals != (TimelineTotals{}) {
		t.Errorf("Totals = %+v, want zero", tl.Totals)
	}
}

func TestTimelineBuildPauseResumeMarkers(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 1000, next: "approve"}).
		node(timelineNodeSpec{node: "approve", status: journal.StatusPaused, durationMS: 100}).
		pauseFor(4 * time.Hour).
		node(timelineNodeSpec{node: "code", status: journal.StatusOK, durationMS: 2000}).
		records

	tl := BuildTimeline(TimelineSelection{}, nil, timelineResult(records, false))

	want := []TimelineMarker{
		{AfterIndex: 2, Kind: "paused"},
		{AfterIndex: 2, Kind: "resumed", Gap: 4 * time.Hour},
	}
	got := make([]TimelineMarker, len(tl.Markers))
	for i, m := range tl.Markers {
		got[i] = TimelineMarker{AfterIndex: m.AfterIndex, Kind: m.Kind, Gap: m.Gap}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("markers mismatch (-want +got):\n%s", diff)
	}
}

// timelineWriteRun materializes one run directory under workspace: a real
// journal written through journal.Append (so the on-disk bytes are exactly
// what a run produces) and a manifest.
func timelineWriteRun(t *testing.T, workspace, runID, goal string, createdAt time.Time, records []journal.Record) string {
	t.Helper()

	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		t.Fatalf("NewLayout(%q, %q): %v", workspace, runID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", layout.RunDir(), err)
	}

	j, err := journal.Open(layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	for _, rec := range records {
		rec.Seq = 0 // Append assigns it.
		if _, err := j.Append(rec); err != nil {
			t.Fatalf("journal.Append(%+v): %v", rec, err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal.Close: %v", err)
	}

	m := state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		RunID:         runID,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		Workspace:     workspace,
		Goal:          goal,
		Status:        state.RunStatusCompleted,
		Budget:        state.Budget{LimitUSD: 5},
	}
	if err := state.SaveManifest(layout.ManifestPath(), m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	return layout.JournalPath()
}

func TestTimelineResolveRunPicksMostRecentWhenIDOmitted(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260901T090000Z-aaaaaaaaaaaa", "older goal",
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), timelineFixLoopRun())
	timelineWriteRun(t, ws, "20260905T142530Z-bbbbbbbbbbbb", "newer goal",
		time.Date(2026, 9, 5, 14, 25, 30, 0, time.UTC), timelineFixLoopRun())

	sel, err := ResolveTimelineRun(ws, "")
	if err != nil {
		t.Fatalf("ResolveTimelineRun: %v", err)
	}
	if got, want := sel.RunID, "20260905T142530Z-bbbbbbbbbbbb"; got != want {
		t.Errorf("RunID = %q, want %q", got, want)
	}
	if !sel.Implicit {
		t.Error("Implicit = false, want true when no run id was given")
	}
	if got, want := sel.TotalRuns, 2; got != want {
		t.Errorf("TotalRuns = %d, want %d", got, want)
	}
}

func TestTimelineResolveRunExplicitID(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260901T090000Z-aaaaaaaaaaaa", "older goal",
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), timelineFixLoopRun())
	timelineWriteRun(t, ws, "20260905T142530Z-bbbbbbbbbbbb", "newer goal",
		time.Date(2026, 9, 5, 14, 25, 30, 0, time.UTC), timelineFixLoopRun())

	sel, err := ResolveTimelineRun(ws, "20260901T090000Z-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("ResolveTimelineRun: %v", err)
	}
	if got, want := sel.RunID, "20260901T090000Z-aaaaaaaaaaaa"; got != want {
		t.Errorf("RunID = %q, want %q", got, want)
	}
	if sel.Implicit {
		t.Error("Implicit = true, want false when the id was given explicitly")
	}
}

func TestTimelineResolveRunUnknownIDExplainsItself(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260901T090000Z-aaaaaaaaaaaa", "a goal",
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), timelineFixLoopRun())
	timelineWriteRun(t, ws, "20260902T090000Z-cccccccccccc", "another goal",
		time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC), timelineFixLoopRun())

	_, err := ResolveTimelineRun(ws, "not-a-real-run")
	if !errors.Is(err, ErrTimelineRunNotFound) {
		t.Fatalf("error = %v, want one matching ErrTimelineRunNotFound", err)
	}

	var nf *TimelineRunNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error %v is not a *TimelineRunNotFoundError", err)
	}
	if nf.Available != 2 {
		t.Errorf("Available = %d, want 2", nf.Available)
	}

	msg := err.Error()
	for _, want := range []string{`"not-a-real-run"`, "belay runs", "2 runs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q does not mention %q", msg, want)
		}
	}
}

func TestTimelineResolveRunEmptyWorkspace(t *testing.T) {
	t.Parallel()

	_, err := ResolveTimelineRun(t.TempDir(), "")
	if !errors.Is(err, ErrTimelineRunNotFound) {
		t.Fatalf("error = %v, want one matching ErrTimelineRunNotFound", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "belay run") {
		t.Errorf("error message %q should suggest starting a run", msg)
	}
}

func TestTimelineResolveRunRejectsTraversal(t *testing.T) {
	t.Parallel()

	if _, err := ResolveTimelineRun(t.TempDir(), "../../etc"); err == nil {
		t.Fatal("ResolveTimelineRun accepted a traversing run id, want an error")
	}
}

func TestTimelineLoadFromDisk(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260905T142530Z-bbbbbbbbbbbb", "add pagination to the users endpoint",
		timelineBase, timelineFixLoopRun())

	tl, err := LoadTimeline(ws, "")
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if got, want := tl.Goal, "add pagination to the users endpoint"; got != want {
		t.Errorf("Goal = %q, want %q", got, want)
	}
	if got, want := tl.Totals.BudgetLimitUSD, 5.0; got != want {
		t.Errorf("BudgetLimitUSD = %v, want %v", got, want)
	}
	if got, want := len(tl.Entries), 10; got != want {
		t.Errorf("len(Entries) = %d, want %d", got, want)
	}
	if got, want := tl.Totals.AgentDuration, 319700*time.Millisecond; got != want {
		t.Errorf("AgentDuration = %v, want %v", got, want)
	}
}

// TestTimelineLoadTruncatedJournal truncates a valid journal mid-line, which
// is exactly what a kill during journal.Append leaves behind, and proves the
// timeline still renders every complete record and reports the interruption.
func TestTimelineLoadTruncatedJournal(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	// A run that dies while "fix" is executing: the fix node's node_started
	// is durable, its node_finished was still being written.
	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 12400, next: "code",
			usage: usd(4200, 1800, 0.031, false)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 33100, next: "fix",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 41700, next: "test",
			usage: usd(12000, 5400, 0.180, false)}).
		records
	path := timelineWriteRun(t, ws, "20260905T142530Z-dddddddddddd", "fix the failing tests",
		timelineBase, records)

	timelineTruncateAt(t, path, 20)

	tl, err := LoadTimeline(ws, "")
	if err != nil {
		t.Fatalf("LoadTimeline on a truncated journal returned an error: %v", err)
	}

	// Every complete record still renders: plan, test, and the fix node that
	// started but whose completion was lost with the torn line.
	wantNodes := []string{"plan", "test", "fix"}
	gotNodes := make([]string, 0, len(tl.Entries))
	for _, e := range tl.Entries {
		gotNodes = append(gotNodes, e.Node)
	}
	if diff := cmp.Diff(wantNodes, gotNodes); diff != "" {
		t.Errorf("entries mismatch (-want +got):\n%s", diff)
	}
	if !tl.Interrupted || !tl.InterruptedMidWrite {
		t.Errorf("Interrupted = %v, InterruptedMidWrite = %v, want true/true",
			tl.Interrupted, tl.InterruptedMidWrite)
	}
	if got, want := tl.InterruptedNode, "fix"; got != want {
		t.Errorf("InterruptedNode = %q, want %q", got, want)
	}
	if got := tl.Entries[len(tl.Entries)-1].Outcome; got != TimelineInterrupted {
		t.Errorf("last entry outcome = %q, want %q", got, TimelineInterrupted)
	}
	// The two nodes that did finish still contribute their real cost.
	if got, want := tl.Totals.USD, 0.035; math.Abs(got-want) > 1e-9 {
		t.Errorf("USD = %v, want %v", got, want)
	}
}

// timelineDemoManifest is the manifest the render tests pair with
// timelineFixLoopRun, so the rendered goal and budget ceiling are fixed.
func timelineDemoManifest() *state.Manifest {
	return &state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		RunID:         "20260905T142530Z-9f3a21bc4d7e",
		Goal:          "add cursor pagination to the users endpoint",
		Status:        state.RunStatusCompleted,
		Budget:        state.Budget{LimitUSD: 5},
	}
}

// timelineDemoSelection is the fixed selection the render tests use, so the
// golden output does not depend on a temporary directory's name.
func timelineDemoSelection() TimelineSelection {
	return TimelineSelection{
		RunID:     "20260905T142530Z-9f3a21bc4d7e",
		Workspace: "/w",
		Dir:       "/w/.belay/runs/20260905T142530Z-9f3a21bc4d7e",
		Implicit:  true,
		TotalRuns: 3,
	}
}

// timelineGolden pins the rendered timeline for the fix-loop fixture.
//
// ADR 0010 makes output strings part of the interface, so this is a full
// golden rather than a scatter of substring checks: changing any of this
// wording should be a deliberate act with a visible diff.
const timelineGolden = "run     20260905T142530Z-9f3a21bc4d7e\n" +
	"        most recent of 3 runs — `belay runs` lists the rest\n" +
	"goal    add cursor pagination to the users endpoint\n" +
	"status  finished\n" +
	"\n" +
	"   #  node       outcome   duration     cost\n" +
	"   1  plan       finished     12.4s   $0.031  ██\n" +
	"   2  approve    finished      0.2s        –  █\n" +
	"   3  code       finished  1m 48.0s   $0.412  ██████████████████\n" +
	"   4  write      finished      0.9s        –  █\n" +
	"   5  test #1/3  failed       33.1s   $0.004  ██████\n" +
	"      3 of 41 tests failing in pagination_test.go\n" +
	"   6  fix #1/2   finished     41.7s  ~$0.180  ███████\n" +
	"   7  test #2/3  failed       31.9s   $0.004  █████\n" +
	"      1 of 41 tests failing in pagination_test.go\n" +
	"   8  fix #2/2   finished     38.2s   $0.201  ██████\n" +
	"   9  test #3/3  finished     30.5s   $0.004  █████\n" +
	"  10  review     finished     22.8s   $0.092  ████\n" +
	"\n" +
	"  loops       test ran 3 times, fix ran 2 times\n" +
	"  loop cost   steps 5-9 cost $0.393 and 2m 55.4s — 42% of the run's spend\n" +
	"  agent time  5m 19.7s across 10 nodes\n" +
	"  wall clock  5m 30.7s   2026-09-05 14:25:31Z → 14:31:01Z\n" +
	"  spend       $0.928 of the $5.00 ceiling\n" +
	"              estimated — at least one node's cost was a heuristic, not a reported charge\n"

func timelineRender(t *testing.T, tl Timeline, color bool) string {
	t.Helper()
	var b strings.Builder
	if err := RenderTimeline(&b, tl, color); err != nil {
		t.Fatalf("RenderTimeline: %v", err)
	}
	return b.String()
}

func TestTimelineRenderGolden(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))

	if diff := cmp.Diff(timelineGolden, timelineRender(t, tl, false)); diff != "" {
		t.Errorf("rendered timeline mismatch (-want +got):\n%s", diff)
	}
}

// TestTimelineRenderNumbersLoopIterations is the loop-visibility contract
// stated as an assertion: three test runs and two repairs must be legible
// without counting rows.
func TestTimelineRenderNumbersLoopIterations(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))
	out := timelineRender(t, tl, false)

	for _, want := range []string{
		"test #1/3", "test #2/3", "test #3/3", "fix #1/2", "fix #2/2",
		"loops       test ran 3 times, fix ran 2 times",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// A node that ran once must NOT be numbered: numbering everything would
	// destroy the signal that numbering carries.
	if strings.Contains(out, "plan #") || strings.Contains(out, "review #") {
		t.Errorf("single-execution nodes should not be numbered:\n%s", out)
	}
}

// TestTimelineRenderTotalsRow checks every total against the values computed
// by hand in timelineFixLoopRun's doc comment.
func TestTimelineRenderTotalsRow(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))
	out := timelineRender(t, tl, false)

	for _, want := range []string{
		"agent time  5m 19.7s across 10 nodes", // 319700ms
		"wall clock  5m 30.7s",                 // 330700ms
		"spend       $0.928 of the $5.00 ceiling",
		"estimated — at least one node's cost was a heuristic",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("totals block does not contain %q:\n%s", want, out)
		}
	}
}

// TestTimelineRenderUsesReadersVocabulary guards the language rule: no type
// names, no raw event or status spellings.
func TestTimelineRenderUsesReadersVocabulary(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 1000, next: "code"}).
		node(timelineNodeSpec{node: "code", status: journal.StatusAborted, durationMS: 2000}).
		run(journal.EventRunAborted).
		records
	tl := BuildTimeline(timelineDemoSelection(), nil, timelineResult(records, false))
	out := timelineRender(t, tl, false)

	if !strings.Contains(out, "stopped") {
		t.Errorf("an aborted node should read as %q:\n%s", "stopped", out)
	}
	for _, banned := range []string{"StatusAborted", "RunStateAborted", "node_finished", "run_aborted", "aborted"} {
		if strings.Contains(out, banned) {
			t.Errorf("output leaks the type system's vocabulary %q:\n%s", banned, out)
		}
	}
}

func TestTimelineRenderRerunAfterInterruptionSaysSoInWords(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "code", attempt: 3, status: journal.StatusOK, durationMS: 2000}).
		records
	tl := BuildTimeline(timelineDemoSelection(), nil, timelineResult(records, false))
	out := timelineRender(t, tl, false)

	if !strings.Contains(out, "re-ran after an interrupted attempt 2") {
		t.Errorf("a re-run node should say so in words, not just show a number:\n%s", out)
	}
}

func TestTimelineRenderEmptyRun(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(), journal.ReadResult{})
	out := timelineRender(t, tl, false)

	if !strings.Contains(out, "has not recorded any nodes yet") {
		t.Errorf("an empty run needs an empty state, got:\n%s", out)
	}
}

func TestTimelineRenderNamesTheRunItChose(t *testing.T) {
	t.Parallel()

	implicit := BuildTimeline(timelineDemoSelection(), nil, timelineResult(timelineFixLoopRun(), false))
	if out := timelineRender(t, implicit, false); !strings.Contains(out, "most recent of 3 runs") {
		t.Errorf("an implicit target must be named as such:\n%s", out)
	}

	sel := timelineDemoSelection()
	sel.Implicit = false
	explicit := BuildTimeline(sel, nil, timelineResult(timelineFixLoopRun(), false))
	if out := timelineRender(t, explicit, false); strings.Contains(out, "most recent of") {
		t.Errorf("an explicitly named run must not claim it was chosen implicitly:\n%s", out)
	}
}

func TestTimelineRenderTruncatedJournalReportsInterruption(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 12400, next: "test",
			usage: usd(4200, 1800, 0.031, false)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 33100, next: "fix",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 41700, next: "test",
			usage: usd(12000, 5400, 0.180, false)}).
		records
	path := timelineWriteRun(t, ws, "20260905T142530Z-dddddddddddd", "fix the tests", timelineBase, records)
	timelineTruncateAt(t, path, 20)

	tl, err := LoadTimeline(ws, "")
	if err != nil {
		t.Fatalf("LoadTimeline on a truncated journal: %v", err)
	}
	out := timelineRender(t, tl, false)

	// Every complete record still renders.
	for _, want := range []string{"1  plan", "2  test", "3  fix"} {
		if !strings.Contains(out, want) {
			t.Errorf("truncated journal dropped a complete record (%q missing):\n%s", want, out)
		}
	}
	// The interruption is stated as an expected fact, in plain language.
	for _, want := range []string{
		"The run was interrupted while fix was running",
		"only thing lost",
		"durable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("interruption notice missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"corrupt", "malformed", "invalid", "error"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("a torn tail is expected, not damage, but output says %q:\n%s", banned, out)
		}
	}
}

// timelineTruncateAt cuts path partway into its final line, reproducing a
// process killed mid-Append.
func timelineTruncateAt(t *testing.T, path string, into int) {
	t.Helper()

	// path is always a journal inside this test's own t.TempDir, so the
	// taint gosec sees on it carries no external input.
	full, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lastNL := strings.LastIndexByte(strings.TrimRight(string(full), "\n"), '\n')
	if lastNL < 0 {
		t.Fatalf("%s has fewer than two lines", path)
	}
	if err := os.WriteFile(path, full[:lastNL+1+into], 0o600); err != nil { //nolint:gosec // test-owned temp path
		t.Fatalf("truncate %s: %v", path, err)
	}
}

// TestTimelineRenderNoEscapeSequencesWithColorOff is the promise that a
// redirected or piped timeline is clean text.
func TestTimelineRenderNoEscapeSequencesWithColorOff(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 1000, next: "fix"}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 2000, next: "test"}).
		node(timelineNodeSpec{node: "test", status: journal.StatusOK, durationMS: 900}).
		openNode("review", 1).
		records
	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(), timelineResult(records, true))

	plain := timelineRender(t, tl, false)
	if strings.ContainsRune(plain, 0x1b) {
		t.Errorf("color-off output contains an escape sequence:\n%q", plain)
	}

	// The negative above is only meaningful if color-on actually emits them.
	colored := timelineRender(t, tl, true)
	if !strings.Contains(colored, "\x1b[31m") {
		t.Errorf("color-on output should paint a failed row red:\n%q", colored)
	}
	// Turning color off must change nothing but the escapes.
	if got := regexpSGR.ReplaceAllString(colored, ""); got != plain {
		t.Errorf("stripping color changed the layout:\n-want %q\n+got  %q", plain, got)
	}
}

// regexpSGR matches the SGR escape sequences the renderer emits.
var regexpSGR = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestTimelineColorEnabledRespectsNoColorAndNonTTY(t *testing.T) {
	// A character device stands in for a terminal; /dev/null is one on every
	// platform belay builds for, so the positive case is testable without a
	// controlling tty.
	charDevice, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() {
		if err := charDevice.Close(); err != nil {
			t.Errorf("close %s: %v", os.DevNull, err)
		}
	})

	regularFile, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	t.Cleanup(func() {
		if err := regularFile.Close(); err != nil {
			t.Errorf("close temp file: %v", err)
		}
	})

	tests := []struct {
		name    string
		noColor *string
		term    string
		w       io.Writer
		want    bool
	}{
		{name: "character device with no NO_COLOR", term: "xterm-256color", w: charDevice, want: true},
		{name: "NO_COLOR set", noColor: ptr("1"), term: "xterm-256color", w: charDevice, want: false},
		{name: "NO_COLOR present but empty", noColor: ptr(""), term: "xterm-256color", w: charDevice, want: false},
		{name: "dumb terminal", term: "dumb", w: charDevice, want: false},
		{name: "redirected to a regular file", term: "xterm-256color", w: regularFile, want: false},
		{name: "piped to a non-file writer", term: "xterm-256color", w: &strings.Builder{}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			// t.Setenv records the original value and restores it on
			// cleanup, so unsetting afterwards is safe.
			t.Setenv("NO_COLOR", "sentinel")
			if tc.noColor == nil {
				if err := os.Unsetenv("NO_COLOR"); err != nil {
					t.Fatalf("unset NO_COLOR: %v", err)
				}
			} else {
				t.Setenv("NO_COLOR", *tc.noColor)
			}

			if got := TimelineColorEnabled(tc.w); got != tc.want {
				t.Errorf("TimelineColorEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestTimelineJSONShapeIsStable(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))

	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, tl); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	wantKeys := []string{
		"ended_at", "entries", "goal", "interrupted", "interruption", "loops", "markers",
		"next", "next_attempt", "run_dir", "run_id", "schema", "selected", "started_at",
		"status", "total_runs", "totals", "workspace",
	}
	gotKeys := slices.Sorted(maps.Keys(doc))
	if diff := cmp.Diff(wantKeys, gotKeys); diff != "" {
		t.Errorf("top-level keys changed (-want +got):\n%s", diff)
	}

	// Collections are always arrays, so a consumer can iterate unguarded.
	for _, key := range []string{"entries", "markers", "loops"} {
		if b := doc[key]; len(b) == 0 || b[0] != '[' {
			t.Errorf("%q should always be an array, got %s", key, b)
		}
	}

	var typed struct {
		Schema   int    `json:"schema"`
		Selected string `json:"selected"`
		Status   string `json:"status"`
		Entries  []struct {
			Index      int    `json:"index"`
			Node       string `json:"node"`
			Iteration  int    `json:"iteration"`
			Iterations int    `json:"iterations"`
			Outcome    string `json:"outcome"`
			DurationMS *int64 `json:"duration_ms"`
			Cost       *struct {
				USD       float64 `json:"usd"`
				Estimated bool    `json:"estimated"`
			} `json:"cost"`
		} `json:"entries"`
		Loops  []timelineJSONLoop `json:"loops"`
		Totals struct {
			Nodes           int      `json:"nodes"`
			AgentDurationMS int64    `json:"agent_duration_ms"`
			WallDurationMS  int64    `json:"wall_duration_ms"`
			USD             float64  `json:"usd"`
			Estimated       bool     `json:"estimated"`
			TokensIn        int64    `json:"tokens_in"`
			TokensOut       int64    `json:"tokens_out"`
			BudgetLimitUSD  *float64 `json:"budget_limit_usd"`
			LoopUSD         float64  `json:"loop_usd"`
			LoopDurationMS  int64    `json:"loop_duration_ms"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(buf.Bytes(), &typed); err != nil {
		t.Fatalf("decode into the documented shape: %v", err)
	}

	if typed.Schema != TimelineJSONVersion {
		t.Errorf("schema = %d, want %d", typed.Schema, TimelineJSONVersion)
	}
	if typed.Selected != "most-recent" {
		t.Errorf("selected = %q, want %q", typed.Selected, "most-recent")
	}
	if got, want := len(typed.Entries), 10; got != want {
		t.Fatalf("len(entries) = %d, want %d", got, want)
	}

	// Loop iterations are machine-readable, not only rendered.
	seventh := typed.Entries[6]
	if seventh.Node != "test" || seventh.Iteration != 2 || seventh.Iterations != 3 {
		t.Errorf("entry 7 = %s #%d/%d, want test #2/3", seventh.Node, seventh.Iteration, seventh.Iterations)
	}
	if seventh.Outcome != "failed" {
		t.Errorf("entry 7 outcome = %q, want %q", seventh.Outcome, "failed")
	}
	// A node that was never billed is null, not a zero cost: the two are
	// different facts and the shape keeps them apart.
	if typed.Entries[1].Cost != nil {
		t.Errorf("the approve gate was never billed, so cost must be null, got %+v", typed.Entries[1].Cost)
	}
	if c := typed.Entries[5].Cost; c == nil || !c.Estimated {
		t.Errorf("entry 6's cost should be marked estimated, got %+v", c)
	}

	if diff := cmp.Diff([]timelineJSONLoop{{Node: "test", Executions: 3}, {Node: "fix", Executions: 2}},
		typed.Loops); diff != "" {
		t.Errorf("loops mismatch (-want +got):\n%s", diff)
	}
	if got, want := typed.Totals.AgentDurationMS, int64(319700); got != want {
		t.Errorf("totals.agent_duration_ms = %d, want %d", got, want)
	}
	if got, want := typed.Totals.WallDurationMS, int64(330700); got != want {
		t.Errorf("totals.wall_duration_ms = %d, want %d", got, want)
	}
	if got, want := typed.Totals.USD, 0.928; math.Abs(got-want) > 1e-9 {
		t.Errorf("totals.usd = %v, want %v", got, want)
	}
	if !typed.Totals.Estimated {
		t.Error("totals.estimated = false, want true")
	}
	if typed.Totals.BudgetLimitUSD == nil || *typed.Totals.BudgetLimitUSD != 5 {
		t.Errorf("totals.budget_limit_usd = %v, want 5", typed.Totals.BudgetLimitUSD)
	}
}

func TestTimelineJSONReportsInterruption(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "plan", status: journal.StatusOK, durationMS: 1000, next: "code"}).
		openNode("code", 1).
		records
	tl := BuildTimeline(timelineDemoSelection(), nil, timelineResult(records, true))

	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, tl); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}

	var typed struct {
		Interrupted  bool `json:"interrupted"`
		Interruption *struct {
			Node     string `json:"node"`
			MidWrite bool   `json:"mid_write"`
			Message  string `json:"message"`
		} `json:"interruption"`
		Entries []struct {
			Outcome    string `json:"outcome"`
			DurationMS *int64 `json:"duration_ms"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(buf.Bytes(), &typed); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !typed.Interrupted || typed.Interruption == nil {
		t.Fatalf("interrupted = %v, interruption = %+v, want true and non-null",
			typed.Interrupted, typed.Interruption)
	}
	if typed.Interruption.Node != "code" || !typed.Interruption.MidWrite {
		t.Errorf("interruption = %+v, want node=code mid_write=true", typed.Interruption)
	}
	if !strings.Contains(typed.Interruption.Message, "durable") {
		t.Errorf("interruption message should describe durability, got %q", typed.Interruption.Message)
	}
	last := typed.Entries[len(typed.Entries)-1]
	if last.Outcome != "interrupted" || last.DurationMS != nil {
		t.Errorf("last entry = %+v, want outcome=interrupted and a null duration", last)
	}
}

// timelineRunCmd executes `belay timeline` with args, capturing its output.
func timelineRunCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	cmd := newTimelineCmd()
	// --workspace is defined once, on the root, so every command names the
	// repository identically. A subcommand built in isolation does not
	// inherit it, so declare the same flag here -- backed by a local, never
	// a package global, because these tests run in parallel.
	var ws string
	cmd.Flags().StringVar(&ws, "workspace", ".", "the repository belay reads and edits")
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestTimelineCommandUnknownRunIDIsHelpful(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260905T142530Z-9f3a21bc4d7e", "a goal", timelineBase, timelineFixLoopRun())

	_, err := timelineRunCmd(t, "20260101T000000Z-deadbeefdead", "--workspace", ws)
	if err == nil {
		t.Fatal("expected an error for an unknown run id")
	}
	if !errors.Is(err, ErrTimelineRunNotFound) {
		t.Errorf("error %v does not match ErrTimelineRunNotFound", err)
	}
	msg := err.Error()
	for _, want := range []string{"20260101T000000Z-deadbeefdead", "belay runs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestTimelineCommandDefaultsToMostRecentAndSaysSo(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260901T090000Z-aaaaaaaaaaaa", "older goal",
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), timelineFixLoopRun())
	timelineWriteRun(t, ws, "20260905T142530Z-9f3a21bc4d7e", "newer goal",
		timelineBase, timelineFixLoopRun())

	out, err := timelineRunCmd(t, "--workspace", ws)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if !strings.Contains(out, "20260905T142530Z-9f3a21bc4d7e") {
		t.Errorf("output does not name the run it chose:\n%s", out)
	}
	if !strings.Contains(out, "most recent of 2 runs") {
		t.Errorf("output does not say the target was chosen implicitly:\n%s", out)
	}
	if !strings.Contains(out, "test #3/3") {
		t.Errorf("output does not number loop iterations:\n%s", out)
	}
}

func TestTimelineCommandJSONFlag(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	timelineWriteRun(t, ws, "20260905T142530Z-9f3a21bc4d7e", "a goal", timelineBase, timelineFixLoopRun())

	out, err := timelineRunCmd(t, "--workspace", ws, "--json")
	if err != nil {
		t.Fatalf("timeline --json: %v", err)
	}

	var doc struct {
		Schema  int    `json:"schema"`
		RunID   string `json:"run_id"`
		Entries []struct {
			Node string `json:"node"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if doc.Schema != TimelineJSONVersion {
		t.Errorf("schema = %d, want %d", doc.Schema, TimelineJSONVersion)
	}
	if doc.RunID != "20260905T142530Z-9f3a21bc4d7e" {
		t.Errorf("run_id = %q", doc.RunID)
	}
	if len(doc.Entries) != 10 {
		t.Errorf("len(entries) = %d, want 10", len(doc.Entries))
	}
}

func TestTimelineCommandRejectsExtraArguments(t *testing.T) {
	t.Parallel()

	if _, err := timelineRunCmd(t, "one", "two"); err == nil {
		t.Fatal("expected an error for two run ids")
	}
}

// TestTimelineCommandIsRegisteredOnRoot proves the subcommand attaches itself
// from this file's init, so root.go never needs editing to add it.
func TestTimelineCommandIsRegisteredOnRoot(t *testing.T) {
	t.Parallel()

	for _, c := range rootCmd.Commands() {
		if c.Name() == "timeline" {
			return
		}
	}
	t.Fatal("`timeline` is not registered on the root command")
}

// TestTimelineLoopCostCoversTheWholeLoopSpan pins the definition of loop
// cost: a repair that worked first time still ran only because a test
// failed, so it is part of what the loop cost even though it never repeated.
func TestTimelineLoopCostCoversTheWholeLoopSpan(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "code", status: journal.StatusOK, durationMS: 108000, next: "test",
			usage: usd(18500, 9200, 0.412, false)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 33100, next: "fix",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 41700, next: "test",
			usage: usd(12000, 5400, 0.180, false)}).
		node(timelineNodeSpec{node: "test", status: journal.StatusOK, durationMS: 30500, next: "review",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "review", status: journal.StatusOK, durationMS: 22800,
			usage: usd(7400, 2600, 0.092, false)}).
		run(journal.EventRunCompleted).
		records
	tl := BuildTimeline(timelineDemoSelection(), nil, timelineResult(records, false))

	// Only "test" repeats, but the loop ran steps 2 through 4:
	// test $0.004 + fix $0.180 + test $0.004 = $0.188.
	if got, want := tl.Totals.LoopUSD, 0.188; math.Abs(got-want) > 1e-9 {
		t.Errorf("LoopUSD = %v, want %v — the repair between two test runs belongs to the loop", got, want)
	}
	if got, want := tl.Totals.LoopDuration, 105300*time.Millisecond; got != want {
		t.Errorf("LoopDuration = %v, want %v", got, want)
	}
	if got, want := [2]int{tl.Totals.LoopFirstIndex, tl.Totals.LoopLastIndex}, [2]int{2, 4}; got != want {
		t.Errorf("loop span = %v, want %v", got, want)
	}

	out := timelineRender(t, tl, false)
	if !strings.Contains(out, "loop cost   steps 2-4 cost $0.188") {
		t.Errorf("loop cost should point at the rows it came from:\n%s", out)
	}
}

// TestTimelineRenderHasNoTrailingWhitespace keeps the output diffable and
// clean in a file: a row whose bar is empty must not pad out to the bar's
// column with spaces.
func TestTimelineRenderHasNoTrailingWhitespace(t *testing.T) {
	t.Parallel()

	records := newTimelineBuilder().
		run(journal.EventRunStarted).
		node(timelineNodeSpec{node: "test", status: journal.StatusFailed, durationMS: 33100, next: "fix",
			usage: usd(900, 210, 0.004, false)}).
		node(timelineNodeSpec{node: "fix", status: journal.StatusOK, durationMS: 41700, next: "test"}).
		node(timelineNodeSpec{node: "test", status: journal.StatusOK, durationMS: 30500, next: "review"}).
		openNode("review", 1).
		records
	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(), timelineResult(records, true))

	for i, line := range strings.Split(timelineRender(t, tl, false), "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
}
