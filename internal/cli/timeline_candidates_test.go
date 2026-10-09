package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/google/go-cmp/cmp"
)

// timelineCandidateRun is one candidate run to synthesize on disk: its
// manifest block, the blackboard it ended with, and its own journal.
type timelineCandidateRun struct {
	block   state.CandidateRun
	final   state.State
	records []journal.Record
	// noManifest models a candidate directory a torn claim left behind.
	noManifest bool
	// tornTail appends an incomplete final line, which is what a journal
	// looks like when the process was killed between write and flush.
	tornTail bool
}

// timelineWriteParentRun writes a parent fanout run: a manifest carrying the
// fanout pin, a blackboard, and a journal whose fanout node is the last
// thing in it.
func timelineWriteParentRun(t *testing.T, workspace, runID string, pin state.FanoutPin,
	st state.State, records []journal.Record,
) state.Layout {
	t.Helper()

	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		t.Fatalf("NewLayout(%q, %q): %v", workspace, runID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", layout.RunDir(), err)
	}

	m := state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		RunID:         runID,
		CreatedAt:     timelineBase,
		UpdatedAt:     timelineBase,
		Workspace:     workspace,
		Goal:          "make the thing fast",
		Status:        state.RunStatusCompleted,
		Budget:        state.Budget{LimitUSD: 5},
	}
	m.SetFanout(pin)
	if err := state.SaveManifest(layout.ManifestPath(), m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	if err := state.SaveState(layout.StatePath(), st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	timelineAppend(t, layout.JournalPath(), records, false)
	return layout
}

// timelineWriteCandidateRun writes one candidate's own run directory under
// parent, exactly as the candidate runner leaves it: its own manifest with a
// candidate block, its own blackboard, and its own journal.
func timelineWriteCandidateRun(t *testing.T, parent state.Layout, parentRunID string, c timelineCandidateRun) state.Layout {
	t.Helper()

	layout, err := state.NewCandidateRunLayout(parent, c.block.ID)
	if err != nil {
		t.Fatalf("NewCandidateRunLayout(%q): %v", c.block.ID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", layout.RunDir(), err)
	}

	if !c.noManifest {
		m := state.Manifest{
			SchemaVersion: state.ManifestSchemaVersion,
			RunID:         parentRunID + "-" + c.block.ID,
			CreatedAt:     timelineBase,
			UpdatedAt:     timelineBase,
			Workspace:     parent.WorkspaceDir(),
			Goal:          "make the thing fast",
			Status:        state.RunStatusRunning,
			Budget:        state.Budget{LimitUSD: c.block.Envelope.LimitUSD},
		}
		m.SetCandidate(c.block)
		if err := state.SaveManifest(layout.ManifestPath(), m); err != nil {
			t.Fatalf("SaveManifest(%s): %v", c.block.ID, err)
		}
	}
	final := c.final
	if final.SchemaVersion == 0 {
		// A zero State is not a blackboard any belay wrote; give a
		// candidate with nothing to say a valid empty one.
		final = state.NewState("make the thing fast")
	}
	if err := state.SaveState(layout.StatePath(), final); err != nil {
		t.Fatalf("SaveState(%s): %v", c.block.ID, err)
	}
	timelineAppend(t, layout.JournalPath(), c.records, c.tornTail)
	return layout
}

// workspaceOf is the parent layout's workspace, which a candidate manifest
// records as its own.
// timelineAppend writes records to a journal, optionally leaving a torn
// final line behind.
func timelineAppend(t *testing.T, path string, records []journal.Record, torn bool) {
	t.Helper()

	j, err := journal.Open(path)
	if err != nil {
		t.Fatalf("journal.Open(%s): %v", path, err)
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
	if !torn {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the fixture journal this test just wrote
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	if _, err := f.WriteString(`{"seq":99,"event":"node_star`); err != nil {
		t.Fatalf("write a torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// timelineCandidateJournal is one candidate's own journal: a short run that
// ends with the marker given.
func timelineCandidateJournal(end journal.Event, spend float64, failed int) []journal.Record {
	b := newTimelineBuilder().run(journal.EventRunStarted)
	b.node(timelineNodeSpec{node: "code", status: journal.StatusOK, durationMS: 20000, next: "write",
		usage: usd(9000, 3000, spend, false), note: "code: 2 file(s)"})
	b.node(timelineNodeSpec{node: "write", status: journal.StatusOK, durationMS: 100, next: "test",
		usage: usd(0, 0, 0, false), note: "verified 2 changed file(s)"})
	status := journal.StatusOK
	if failed > 0 {
		status = journal.StatusFailed
	}
	b.node(timelineNodeSpec{node: "test", status: status, durationMS: 4000,
		usage: usd(0, 0, 0, false), note: "go-test: 12 tests, 1 failed"})
	if end != journal.EventUnknown {
		b.run(end)
	}
	return b.records
}

// timelineSettledFixture is a fanout run whose fanout node returned: the
// parent blackboard carries every candidate's settled figures and the
// winner.
func timelineSettledFixture() state.State {
	st := state.NewState("make the thing fast")
	st.Candidates = []state.Candidate{
		{
			ID: "cand-01", Dir: "/ws/.belay/runs/p/candidates/cand-01",
			TestPassed: false, IssueCount: 4, Ephemeral: true,
			Outcome: "failed", RunDir: "candidate-runs/cand-01",
			SpentUSD: 0.310, DurationMS: 24100, Note: "the fix loop gave up",
		},
		{
			ID: "cand-02", Dir: "/ws/.belay/runs/p/candidates/cand-02",
			TestPassed: true, IssueCount: 1, Ephemeral: true,
			Outcome: "completed", RunDir: "candidate-runs/cand-02",
			SpentUSD: 0.420, Estimated: true, DurationMS: 65000, Note: "",
		},
		{
			ID: "cand-03", Dir: "", TestPassed: false, IssueCount: 0,
			Outcome: "error", RunDir: "candidate-runs/cand-03",
			SpentUSD: 0, DurationMS: 0,
			Note: "the isolator refused this workspace: the source is not a git repository",
		},
	}
	st.Winner = "cand-02"
	return st
}

// TestTimelineLanesComeFromTheSettledBlackboard pins where a finished
// fanout's figures come from: once the fanout node returns, the parent
// blackboard holds what the run was actually billed for, and the lanes must
// agree with the parent's own spend line rather than re-deriving numbers
// from journals a promote may since have cleaned up around.
func TestTimelineLanesComeFromTheSettledBlackboard(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	const runID = "20260905T142530Z-aaaaaaaaaaaa"
	timelineWriteParentRun(t, ws, runID, state.FanoutPin{Candidates: 3, Select: "fewest_issues"},
		timelineSettledFixture(), timelineFixLoopRun())

	tl, err := LoadTimeline(ws, runID)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}

	if got, want := tl.Winner, "cand-02"; got != want {
		t.Errorf("Winner = %q, want %q", got, want)
	}
	want := []TimelineCandidate{
		{
			ID: "cand-01", Index: 1, RunID: runID + "-cand-01", RunDir: "candidate-runs/cand-01",
			WorkspaceDir: "/ws/.belay/runs/p/candidates/cand-01",
			Outcome:      TimelineCandidateFailed, Settled: true,
			IssueCount: 4, SpentUSD: 0.310, Duration: 24100 * time.Millisecond,
			Note: "the fix loop gave up",
		},
		{
			ID: "cand-02", Index: 2, RunID: runID + "-cand-02", RunDir: "candidate-runs/cand-02",
			WorkspaceDir: "/ws/.belay/runs/p/candidates/cand-02",
			Outcome:      TimelineCandidateFinished, Winner: true, Settled: true,
			TestPassed: true, IssueCount: 1, SpentUSD: 0.420, Estimated: true,
			Duration: 65000 * time.Millisecond,
		},
		{
			ID: "cand-03", Index: 3, RunID: runID + "-cand-03", RunDir: "candidate-runs/cand-03",
			Outcome: TimelineCandidateRefused, Settled: true,
			Note: "the isolator refused this workspace: the source is not a git repository",
		},
	}
	if diff := cmp.Diff(want, tl.Candidates); diff != "" {
		t.Errorf("candidate lanes mismatch (-want +got):\n%s", diff)
	}
}

// TestTimelineLanesAreReadLiveWhileTheFanoutIsOpen pins the other half: a
// fanout that has not settled leaves the parent blackboard empty on purpose
// (the node's Patch is empty so join never sees a partial field), so the
// lanes are read from each candidate's own run — the manifest for its
// identity and envelope, the journal for what it has spent so far.
func TestTimelineLanesAreReadLiveWhileTheFanoutIsOpen(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	const runID = "20260905T142530Z-bbbbbbbbbbbb"
	parent := timelineWriteParentRun(t, ws, runID, state.FanoutPin{Candidates: 3, Select: "fewest_issues"},
		state.NewState("make the thing fast"), timelineFixLoopRun())

	passed := state.NewState("make the thing fast")
	passed.Test = state.Test{Total: 12, Passed: 12}
	passed.Review.Issues = []belay.Issue{{Severity: belay.SeverityMajor, Message: "one"}}

	// Written out of index order, to prove the lanes are ordered by the
	// index each candidate's manifest records and not by directory order.
	for _, c := range []timelineCandidateRun{
		{
			block: state.CandidateRun{
				ParentRunID: runID, ID: "cand-03", Index: 3,
				Provision: state.ProvisionRefused, ProvisionNote: "the source is not a git repository",
				Envelope: state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
			},
		},
		{
			block: state.CandidateRun{
				ParentRunID: runID, ID: "cand-01", Index: 1,
				Provision: state.ProvisionOK, Envelope: state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
				Workspace: belay.Workspace{Dir: filepath.Join(parent.CandidatesDir(), "cand-01"), Ephemeral: true},
			},
			final:   passed,
			records: timelineCandidateJournal(journal.EventRunCompleted, 0.25, 0),
		},
		{
			block: state.CandidateRun{
				ParentRunID: runID, ID: "cand-02", Index: 2,
				Provision: state.ProvisionOK, Envelope: state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
				Workspace: belay.Workspace{Dir: filepath.Join(parent.CandidatesDir(), "cand-02"), Ephemeral: true},
			},
			final:   state.NewState("make the thing fast"),
			records: timelineCandidateJournal(journal.EventUnknown, 0.11, 1),
		},
	} {
		timelineWriteCandidateRun(t, parent, runID, c)
	}

	tl, err := LoadTimeline(ws, runID)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if tl.Winner != "" {
		t.Errorf("Winner = %q, want empty: nothing has selected one yet", tl.Winner)
	}

	want := []TimelineCandidate{
		{
			ID: "cand-01", Index: 1, RunID: runID + "-cand-01", RunDir: "candidate-runs/cand-01",
			WorkspaceDir: filepath.Join(parent.CandidatesDir(), "cand-01"),
			Outcome:      TimelineCandidateFinished,
			TestPassed:   true, IssueCount: 1,
			Provision: state.ProvisionOK,
			Envelope:  state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
			SpentUSD:  0.25, Duration: 24100 * time.Millisecond, Nodes: 3,
		},
		{
			ID: "cand-02", Index: 2, RunID: runID + "-cand-02", RunDir: "candidate-runs/cand-02",
			WorkspaceDir: filepath.Join(parent.CandidatesDir(), "cand-02"),
			Outcome:      TimelineCandidateUnfinished,
			Provision:    state.ProvisionOK,
			Envelope:     state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
			SpentUSD:     0.11, Duration: 24100 * time.Millisecond, Nodes: 3,
		},
		{
			ID: "cand-03", Index: 3, RunID: runID + "-cand-03", RunDir: "candidate-runs/cand-03",
			Outcome:       TimelineCandidateRefused,
			Provision:     state.ProvisionRefused,
			ProvisionNote: "the source is not a git repository",
			Envelope:      state.Envelope{LimitUSD: 1.5, LimitTokens: 60000},
			Note:          "the source is not a git repository",
		},
	}
	if diff := cmp.Diff(want, tl.Candidates); diff != "" {
		t.Errorf("live candidate lanes mismatch (-want +got):\n%s", diff)
	}
}

// TestTimelineOfARunWithoutFanoutHasNoLanes is the guard that keeps every
// run anybody has today printing exactly what it prints today: no fanout
// pin, no candidate directories, no lanes, no winner.
func TestTimelineOfARunWithoutFanoutHasNoLanes(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	const runID = "20260905T142530Z-cccccccccccc"
	timelineWriteRun(t, ws, runID, "make the thing fast", timelineBase, timelineFixLoopRun())

	tl, err := LoadTimeline(ws, runID)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if len(tl.Candidates) != 0 {
		t.Errorf("Candidates = %+v, want none", tl.Candidates)
	}
	if tl.Winner != "" {
		t.Errorf("Winner = %q, want empty", tl.Winner)
	}
}

// TestTimelineNeverRewritesACandidateJournal is the rule that keeps a reader
// from corrupting a run: journal.Open heals a torn tail by rewriting the
// file, and a candidate's journal belongs to a goroutine that may still be
// writing it. Reading it must leave every byte where it was, and must report
// the candidate as interrupted rather than tidying the evidence away.
func TestTimelineNeverRewritesACandidateJournal(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	const runID = "20260905T142530Z-dddddddddddd"
	parent := timelineWriteParentRun(t, ws, runID, state.FanoutPin{Candidates: 1, Select: "fewest_issues"},
		state.NewState("make the thing fast"), timelineFixLoopRun())

	// A journal whose last node_started never got its node_finished, and
	// whose final line was itself cut in half: both halves of a kill -9.
	open := newTimelineBuilder().
		run(journal.EventRunStarted)
	open.node(timelineNodeSpec{node: "code", status: journal.StatusOK, durationMS: 20000, next: "write",
		usage: usd(9000, 3000, 0.19, false)})
	open.add(journal.Record{Event: journal.EventNodeStarted, Node: "write", Attempt: 1})

	cand := timelineWriteCandidateRun(t, parent, runID, timelineCandidateRun{
		block: state.CandidateRun{
			ParentRunID: runID, ID: "cand-01", Index: 1, Provision: state.ProvisionOK,
			Envelope: state.Envelope{LimitUSD: 5},
		},
		final:    state.NewState("make the thing fast"),
		records:  open.records,
		tornTail: true,
	})

	before := timelineDigest(t, cand.JournalPath())

	tl, err := LoadTimeline(ws, runID)
	if err != nil {
		t.Fatalf("LoadTimeline: %v", err)
	}
	if got := timelineDigest(t, cand.JournalPath()); got != before {
		t.Errorf("belay timeline rewrote a candidate journal: %s -> %s", before, got)
	}
	if len(tl.Candidates) != 1 {
		t.Fatalf("Candidates = %+v, want one lane", tl.Candidates)
	}
	if got, want := tl.Candidates[0].Outcome, TimelineCandidateInterrupted; got != want {
		t.Errorf("lane outcome = %q, want %q", got, want)
	}
	if !tl.Candidates[0].Interrupted {
		t.Error("lane Interrupted = false, want true for a journal with an open node and a torn tail")
	}

	// --candidate opens the same journal, and must be just as read-only.
	ctl, err := LoadCandidateTimeline(ws, runID, "cand-01")
	if err != nil {
		t.Fatalf("LoadCandidateTimeline: %v", err)
	}
	if got := timelineDigest(t, cand.JournalPath()); got != before {
		t.Errorf("belay timeline --candidate rewrote a candidate journal: %s -> %s", before, got)
	}
	if !ctl.Interrupted {
		t.Error("the candidate's own timeline should report the interruption")
	}
	if got, want := ctl.Run.Candidate, "cand-01"; got != want {
		t.Errorf("selection Candidate = %q, want %q", got, want)
	}
}

// timelineDigest is the sha256 of a file, so a test can prove a reader left
// it alone byte for byte.
func timelineDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a fixture path this test built
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(sha256.New().Sum(b))
}

// timelineSettledLanesFixture is the settled fanout's lanes as the reader
// model holds them: one failed, one finished and winning with an estimated
// figure, one refused before it ever ran.
func timelineSettledLanesFixture() []TimelineCandidate {
	return []TimelineCandidate{
		{
			ID: "cand-01", Index: 1, RunDir: "candidate-runs/cand-01",
			Outcome: TimelineCandidateFailed, Settled: true,
			IssueCount: 4, SpentUSD: 0.310, Duration: 24100 * time.Millisecond,
			Note: "the fix loop gave up",
		},
		{
			ID: "cand-02", Index: 2, RunDir: "candidate-runs/cand-02",
			Outcome: TimelineCandidateFinished, Winner: true, Settled: true,
			TestPassed: true, IssueCount: 1, SpentUSD: 0.420, Estimated: true,
			Duration: 65000 * time.Millisecond,
		},
		{
			ID: "cand-03", Index: 3, RunDir: "candidate-runs/cand-03",
			Outcome: TimelineCandidateRefused, Settled: true,
			Note: "the isolator refused this workspace: the source is not a git repository",
		},
	}
}

// timelineLiveLanesFixture is the same fanout while it is still open: the
// figures come from each candidate's own journal and every candidate is
// capped by its own envelope, not by the parent's ceiling.
func timelineLiveLanesFixture() []TimelineCandidate {
	env := state.Envelope{LimitUSD: 1.5, LimitTokens: 60000}
	return []TimelineCandidate{
		{
			ID: "cand-01", Index: 1, RunDir: "candidate-runs/cand-01",
			Outcome: TimelineCandidateFinished, TestPassed: true, IssueCount: 1,
			Provision: state.ProvisionOK, Envelope: env,
			SpentUSD: 0.25, Duration: 24100 * time.Millisecond, Nodes: 3,
		},
		{
			ID: "cand-02", Index: 2, RunDir: "candidate-runs/cand-02",
			Outcome: TimelineCandidateUnfinished, Provision: state.ProvisionOK, Envelope: env,
			SpentUSD: 0.11, Duration: 24100 * time.Millisecond, Nodes: 3,
		},
		{
			ID: "cand-03", Index: 3, RunDir: "candidate-runs/cand-03",
			Outcome: TimelineCandidateRefused, Provision: state.ProvisionRefused,
			ProvisionNote: "the source is not a git repository", Envelope: env,
			Note: "the source is not a git repository",
		},
	}
}

// The candidate section, exactly as belay timeline prints it for each
// fixture. A settled fanout's figures are final; an open one's are running
// totals, and the footer says which it is rather than leaving a reader to
// take a mid-flight number for the bill.
const (
	timelineSettledLanesGolden = "\nCandidates:\n" +
		"  candidate  outcome   tests       issues  duration     cost\n" +
		"  cand-01    failed    not passed       4     24.1s   $0.310\n" +
		"             the fix loop gave up\n" +
		"  cand-02    finished  passed           1  1m 05.0s  ~$0.420  ← winner\n" +
		"  cand-03    refused   –                –         –        –\n" +
		"             the isolator refused this workspace: the source is not a git\n" +
		"             repository\n" +
		"\n" +
		"  3 candidates spent ~$0.730 in total; cand-02 won\n"

	timelineLiveLanesGolden = "\nCandidates:\n" +
		"  candidate  outcome     tests       issues  duration    cost\n" +
		"  cand-01    finished    passed           1     24.1s  $0.250\n" +
		"  cand-02    unfinished  not passed       0     24.1s  $0.110\n" +
		"  cand-03    refused     –                –         –       –\n" +
		"             the source is not a git repository\n" +
		"\n" +
		"  3 candidates have spent $0.360 so far, each capped at $1.50. The fanout has\n" +
		"  not settled, so these are running totals from each candidate's own journal.\n"
)

// TestTimelineRenderShowsCandidateLanes pins the candidate section: a lane
// per candidate in index order, how each ended, whether its tests passed,
// what it spent, how long it took, and which one won.
//
// The third case is the one that matters most: a run that never fanned out
// prints byte for byte what it printed before candidates existed. The lanes
// are an addition to the timeline, not a change to it.
func TestTimelineRenderShowsCandidateLanes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		lanes  []TimelineCandidate
		winner string
		want   string
	}{
		{
			name:  "a settled fanout reports the figures the run was billed for",
			lanes: timelineSettledLanesFixture(), winner: "cand-02",
			want: timelineGolden + timelineSettledLanesGolden,
		},
		{
			name:  "an open fanout reports each candidate's own running total",
			lanes: timelineLiveLanesFixture(),
			want:  timelineGolden + timelineLiveLanesGolden,
		},
		{
			name: "a run without a fanout is unchanged",
			want: timelineGolden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
				timelineResult(timelineFixLoopRun(), false))
			tl.Candidates, tl.Winner = tt.lanes, tt.winner

			plain := timelineRender(t, tl, false)
			if diff := cmp.Diff(tt.want, plain); diff != "" {
				t.Errorf("rendered timeline mismatch (-want +got):\n%s", diff)
			}
			// Colour is decoration only: stripping it leaves the same bytes.
			colored := timelineRender(t, tl, true)
			if got := regexpSGR.ReplaceAllString(colored, ""); got != plain {
				t.Errorf("stripping colour changed the output:\n-want %q\n+got  %q", plain, got)
			}
		})
	}
}

// TestTimelineCandidateLanesShowControlCharactersAsStandIns keeps a
// candidate directory from rewriting the terminal it is printed to. A
// candidate id is a directory name read off disk, and state's segment
// validation refuses path separators and NUL but says nothing about escape
// sequences, so the renderer is the only thing standing between a hostile
// directory name and the reader's screen. The ESC is shown as ␛ rather than
// dropped, as everywhere belay prints text it did not write (ADR-0015).
func TestTimelineCandidateLanesShowControlCharactersAsStandIns(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))
	tl.Candidates = []TimelineCandidate{{
		ID: "cand\x1b[2J-01", Index: 1, Outcome: TimelineCandidateFailed, Settled: true,
		SpentUSD: 0.1, Duration: time.Second,
		Note: "refused\x1b[2J here",
	}}
	tl.Winner = "cand\x1b[2J-01"

	got := timelineRender(t, tl, false)
	if strings.Contains(got, "\x1b") {
		t.Errorf("the rendered timeline carries an escape sequence:\n%q", got)
	}
	for _, want := range []string{"cand␛[2J-01", "refused␛[2J here", "cand␛[2J-01 won", "1 candidate spent"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered timeline is missing %q:\n%s", want, got)
		}
	}
}

// TestTimelineJSONCarriesTheCandidates pins the additive half of the --json
// contract: four new top-level keys and one object per candidate, with the
// same figures the table shows. The document's schema does not change,
// because nothing already in it changes meaning.
func TestTimelineJSONCarriesTheCandidates(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))
	tl.Candidates, tl.Winner = timelineSettledLanesFixture(), "cand-02"
	tl.Candidates[0].RunID = "r-cand-01"
	tl.Candidates[0].WorkspaceDir = "/ws/candidates/cand-01"
	tl.Candidates[0].Provision = state.ProvisionOK
	tl.Candidates[0].Envelope = state.Envelope{LimitUSD: 1.5, LimitTokens: 60000}
	tl.Candidates[0].Nodes = 4

	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, tl); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	// candidates is always an array, so a consumer can iterate unguarded.
	if b := doc["candidates"]; len(b) == 0 || b[0] != '[' {
		t.Errorf("%q should always be an array, got %s", "candidates", b)
	}
	if got, want := string(doc["winner"]), `"cand-02"`; got != want {
		t.Errorf("winner = %s, want %s", got, want)
	}
	for _, key := range []string{"candidate", "parent_run_id"} {
		if got := string(doc[key]); got != "null" {
			t.Errorf("%s = %s, want null for a parent run's own timeline", key, got)
		}
	}

	var lanes []map[string]json.RawMessage
	if err := json.Unmarshal(doc["candidates"], &lanes); err != nil {
		t.Fatalf("decode candidates: %v", err)
	}
	if got, want := len(lanes), 3; got != want {
		t.Fatalf("len(candidates) = %d, want %d", got, want)
	}
	wantKeys := []string{
		"duration_ms", "envelope_tokens", "envelope_usd", "estimated", "id", "index",
		"interrupted", "issue_count", "nodes", "note", "outcome", "provision",
		"provision_note", "run_dir", "run_id", "settled", "spent_usd", "tests_passed",
		"winner", "workspace_dir",
	}
	if diff := cmp.Diff(wantKeys, slices.Sorted(maps.Keys(lanes[0]))); diff != "" {
		t.Errorf("candidate keys changed (-want +got):\n%s", diff)
	}

	var typed []timelineJSONCandidate
	if err := json.Unmarshal(doc["candidates"], &typed); err != nil {
		t.Fatalf("decode candidates into the documented shape: %v", err)
	}
	want := []timelineJSONCandidate{
		{
			ID: "cand-01", Index: 1, Outcome: TimelineCandidateFailed,
			RunID: "r-cand-01", RunDir: "candidate-runs/cand-01",
			WorkspaceDir: "/ws/candidates/cand-01", Settled: true,
			IssueCount: 4, Provision: "ok", EnvelopeUSD: 1.5, EnvelopeTokens: 60000,
			SpentUSD: 0.310, DurationMS: 24100, Nodes: 4, Note: "the fix loop gave up",
		},
		{
			ID: "cand-02", Index: 2, Outcome: TimelineCandidateFinished, Winner: true,
			RunDir: "candidate-runs/cand-02", Settled: true,
			TestPassed: true, IssueCount: 1, SpentUSD: 0.420, Estimated: true, DurationMS: 65000,
		},
		{
			ID: "cand-03", Index: 3, Outcome: TimelineCandidateRefused,
			RunDir: "candidate-runs/cand-03", Settled: true,
			Note: "the isolator refused this workspace: the source is not a git repository",
		},
	}
	if diff := cmp.Diff(want, typed); diff != "" {
		t.Errorf("candidates mismatch (-want +got):\n%s", diff)
	}
}

// TestTimelineJSONOfARunWithoutFanout keeps the shape iterable without
// claiming a fanout: an empty array and three nulls.
func TestTimelineJSONOfARunWithoutFanout(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))

	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, tl); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if got, want := string(doc["candidates"]), "[]"; got != want {
		t.Errorf("candidates = %s, want %s", got, want)
	}
	for _, key := range []string{"winner", "candidate", "parent_run_id"} {
		if got := string(doc[key]); got != "null" {
			t.Errorf("%s = %s, want null", key, got)
		}
	}
}

// TestTimelineJSONNamesTheCandidateItIsAbout makes a --candidate document
// self-describing: a script that is handed one must be able to tell it apart
// from the parent run's without comparing ids by hand.
func TestTimelineJSONNamesTheCandidateItIsAbout(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(TimelineSelection{
		RunID: "r-cand-02", Dir: "/ws/.belay/runs/r/candidate-runs/cand-02",
		Workspace: "/ws", Candidate: "cand-02", ParentRunID: "r", TotalRuns: 1,
	}, nil, timelineResult(timelineFixLoopRun(), false))

	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, tl); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}
	var doc struct {
		RunID       string  `json:"run_id"`
		Candidate   *string `json:"candidate"`
		ParentRunID *string `json:"parent_run_id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.Candidate == nil || *doc.Candidate != "cand-02" {
		t.Errorf("candidate = %v, want %q", doc.Candidate, "cand-02")
	}
	if doc.ParentRunID == nil || *doc.ParentRunID != "r" {
		t.Errorf("parent_run_id = %v, want %q", doc.ParentRunID, "r")
	}
	if got, want := doc.RunID, "r-cand-02"; got != want {
		t.Errorf("run_id = %q, want %q", got, want)
	}
}

// TestTimelineRenderNamesTheCandidateAndItsParent keeps a candidate's own
// timeline from being mistaken for its parent's. They have the same goal, the
// same graph and nearly the same run id, so the header has to say which one
// this is.
func TestTimelineRenderNamesTheCandidateAndItsParent(t *testing.T) {
	t.Parallel()

	tl := BuildTimeline(TimelineSelection{
		RunID: "20260905T142530Z-9f3a21bc4d7e-cand-02", Workspace: "/ws",
		Candidate: "cand-02", ParentRunID: "20260905T142530Z-9f3a21bc4d7e", TotalRuns: 1,
	}, timelineDemoManifest(), timelineResult(timelineFixLoopRun(), false))

	got := timelineRender(t, tl, false)
	const want = "run     20260905T142530Z-9f3a21bc4d7e-cand-02\n" +
		"        candidate cand-02 of 20260905T142530Z-9f3a21bc4d7e\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("header does not name the candidate and its parent:\n-want prefix %q\n+got        %q",
			want, got[:min(len(got), len(want)+40)])
	}
	// The parent's own timeline says nothing about a candidate.
	parent := timelineRender(t, BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false)), false)
	if strings.Contains(parent, "candidate ") {
		t.Errorf("a parent run's header mentions a candidate:\n%s", parent)
	}
}

// timelineFanoutWorkspace writes a parent fanout run with two candidate runs
// under it, and returns the workspace and the parent run id.
func timelineFanoutWorkspace(t *testing.T) (string, string) {
	t.Helper()

	ws := t.TempDir()
	const runID = "20260905T142530Z-eeeeeeeeeeee"
	parent := timelineWriteParentRun(t, ws, runID, state.FanoutPin{Candidates: 2, Select: "fewest_issues"},
		state.NewState("make the thing fast"), timelineFixLoopRun())

	passed := state.NewState("make the thing fast")
	passed.Test = state.Test{Total: 12, Passed: 12}
	for _, c := range []timelineCandidateRun{
		{
			block: state.CandidateRun{
				ParentRunID: runID, ID: "cand-01", Index: 1, Provision: state.ProvisionOK,
				Envelope: state.Envelope{LimitUSD: 2},
			},
			final:   passed,
			records: timelineCandidateJournal(journal.EventRunCompleted, 0.25, 0),
		},
		{
			block: state.CandidateRun{
				ParentRunID: runID, ID: "cand-02", Index: 2, Provision: state.ProvisionOK,
				Envelope: state.Envelope{LimitUSD: 2},
			},
			final:   state.NewState("make the thing fast"),
			records: timelineCandidateJournal(journal.EventRunFailed, 0.11, 1),
		},
	} {
		timelineWriteCandidateRun(t, parent, runID, c)
	}
	return ws, runID
}

// TestTimelineCommandCandidateFlagOpensOneCandidatesTimeline is the whole
// point of --candidate: a candidate is a run, so it renders through the same
// timeline as any other run, with its own nodes rather than the parent's.
func TestTimelineCommandCandidateFlagOpensOneCandidatesTimeline(t *testing.T) {
	t.Parallel()

	ws, runID := timelineFanoutWorkspace(t)

	out, err := timelineRunCmd(t, runID, "--workspace", ws, "--candidate", "cand-02", "--json")
	if err != nil {
		t.Fatalf("timeline --candidate: %v", err)
	}
	var doc struct {
		RunID       string  `json:"run_id"`
		Candidate   *string `json:"candidate"`
		ParentRunID *string `json:"parent_run_id"`
		Status      string  `json:"status"`
		Entries     []struct {
			Node string `json:"node"`
		} `json:"entries"`
		Totals struct {
			USD float64 `json:"usd"`
		} `json:"totals"`
		Candidates []timelineJSONCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if got, want := doc.RunID, runID+"-cand-02"; got != want {
		t.Errorf("run_id = %q, want %q", got, want)
	}
	if doc.Candidate == nil || *doc.Candidate != "cand-02" {
		t.Errorf("candidate = %v, want cand-02", doc.Candidate)
	}
	if doc.ParentRunID == nil || *doc.ParentRunID != runID {
		t.Errorf("parent_run_id = %v, want %q", doc.ParentRunID, runID)
	}
	if got, want := doc.Status, "failed"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	nodes := make([]string, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		nodes = append(nodes, e.Node)
	}
	if diff := cmp.Diff([]string{"code", "write", "test"}, nodes); diff != "" {
		t.Errorf("the candidate's own nodes mismatch (-want +got):\n%s", diff)
	}
	if got, want := doc.Totals.USD, 0.11; got != want {
		t.Errorf("totals.usd = %v, want %v: the candidate's own spend, not the parent's", got, want)
	}
	// A candidate never fans out, so it has no lanes of its own.
	if len(doc.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none for a candidate's own timeline", doc.Candidates)
	}
}

// TestTimelineCommandCandidateFlagNamesTheOnesThatExist makes a mistyped
// candidate id self-correcting: the ids are short and guessable, so seeing
// the real ones is the fix.
func TestTimelineCommandCandidateFlagNamesTheOnesThatExist(t *testing.T) {
	t.Parallel()

	ws, runID := timelineFanoutWorkspace(t)

	_, err := timelineRunCmd(t, runID, "--workspace", ws, "--candidate", "cand-09")
	if err == nil {
		t.Fatal("expected an error for a candidate that does not exist")
	}
	if !errors.Is(err, ErrTimelineCandidateNotFound) {
		t.Errorf("error does not match ErrTimelineCandidateNotFound: %v", err)
	}
	for _, want := range []string{"cand-09", "cand-01", "cand-02", runID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestTimelineCommandCandidateFlagOnARunWithoutFanout says the useful thing
// rather than only reporting a miss: this run has no candidates because it
// never fanned out.
func TestTimelineCommandCandidateFlagOnARunWithoutFanout(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	const runID = "20260905T142530Z-ffffffffffff"
	timelineWriteRun(t, ws, runID, "a goal", timelineBase, timelineFixLoopRun())

	_, err := timelineRunCmd(t, runID, "--workspace", ws, "--candidate", "cand-01")
	if err == nil {
		t.Fatal("expected an error for a run with no candidates")
	}
	if !errors.Is(err, ErrTimelineCandidateNotFound) {
		t.Errorf("error does not match ErrTimelineCandidateNotFound: %v", err)
	}
	if !strings.Contains(err.Error(), "fans out") {
		t.Errorf("error %q does not say why there are no candidates", err)
	}
}

// TestLoadCandidateTimelineRefusesAnEscapingID keeps --candidate from
// resolving anywhere but under the parent's candidate-runs directory.
func TestLoadCandidateTimelineRefusesAnEscapingID(t *testing.T) {
	t.Parallel()

	ws, runID := timelineFanoutWorkspace(t)

	for _, id := range []string{"../../etc", "..", "a/b", ""} {
		if _, err := LoadCandidateTimeline(ws, runID, id); err == nil {
			t.Errorf("LoadCandidateTimeline(%q) returned no error", id)
		}
	}
}

// TestTimelineCommandShowsLanesForARunOnDisk walks the whole path once — the
// command, the run directory, the candidate runs, the renderer — because
// every other test here holds one end of it. It is a guard rather than a RED
// step: the pieces it joins each have their own test above.
func TestTimelineCommandShowsLanesForARunOnDisk(t *testing.T) {
	t.Parallel()

	ws, runID := timelineFanoutWorkspace(t)

	out, err := timelineRunCmd(t, runID, "--workspace", ws)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	const want = "\nCandidates:\n" +
		"  candidate  outcome   tests       issues  duration    cost\n" +
		"  cand-01    finished  passed           0     24.1s  $0.250\n" +
		"  cand-02    failed    not passed       0     24.1s  $0.110\n" +
		"\n" +
		"  2 candidates have spent $0.360 so far, each capped at $2.00. The fanout has\n" +
		"  not settled, so these are running totals from each candidate's own journal.\n"
	if !strings.HasSuffix(out, want) {
		t.Errorf("timeline output does not end with the candidate section:\n-want %q\n+got  %s", want, out)
	}
	// Nothing rewrote either candidate's journal on the way through.
	parent, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	for _, id := range []string{"cand-01", "cand-02"} {
		layout, err := state.NewCandidateRunLayout(parent, id)
		if err != nil {
			t.Fatalf("NewCandidateRunLayout(%q): %v", id, err)
		}
		res, err := journal.ReadFile(layout.JournalPath())
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", id, err)
		}
		if err := journal.Validate(res.Records); err != nil {
			t.Errorf("candidate %q journal no longer validates: %v", id, err)
		}
	}
}
