//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/belay-dev/belay/internal/config"
	"github.com/belay-dev/belay/internal/graph"
	"github.com/belay-dev/belay/internal/journal"
	"github.com/belay-dev/belay/internal/state"
	"github.com/belay-dev/belay/pkg/belay"
)

// TestDurability_CrashInsideNode_ReRunsAtAttempt2 is acceptance scenario 1.
//
// A real process crash is simulated by letting a durabilityNode panic after
// the dispatcher has already durably journalled its node_started record --
// exactly the window a kill -9, a segfault or an unhandled panic inside a
// real node leaves behind, since none of them let node.Run return. A fresh
// Dispatcher (process 2) must then re-run the node, at attempt 2, and finish
// the run.
func TestDurability_CrashInsideNode_ReRunsAtAttempt2(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "survive a crash")

	plan := &durabilityNode{name: graph.NodePlan, steps: []durabilityStep{{crash: true}}}
	code := durabilityOK(graph.NodeCode, graph.End)

	d1 := h.dispatcher(plan, code)
	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d1.Run(context.Background()) })

	// What the crash actually left behind: a valid, resumable journal whose
	// last record is an unmatched node_started -- not corruption, and not a
	// torn write, because the panic happened after that record's Append (and
	// its fsync) had already returned.
	res := h.readJournal()
	if res.Incomplete {
		t.Fatalf("journal reports a torn tail; the injected panic happened after node_started was durably appended")
	}
	if err := journal.Validate(res.Records); err != nil {
		t.Fatalf("journal.Validate() after the crash = %v, want nil", err)
	}
	if got := len(res.Records); got != 2 {
		t.Fatalf("journal has %d records after the crash, want 2 (run_started, plan#1 started): %+v", got, res.Records)
	}
	if code.calls != 0 {
		t.Fatalf("code ran %d times before plan ever finished, want 0", code.calls)
	}

	// Process 2: a fresh Dispatcher over the same run directory, exactly as
	// `belay run` restarting after a crash would construct.
	plan2 := durabilityOK(graph.NodePlan, graph.NodeCode)
	code2 := durabilityOK(graph.NodeCode, graph.End)
	out, err := h.dispatcher(plan2, code2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() after the crash error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}

	if plan2.calls != 1 {
		t.Fatalf("plan ran %d times on resume, want exactly 1: it must be re-run, never skipped", plan2.calls)
	}
	if plan2.attempts[0] != 2 {
		t.Errorf("plan re-ran at attempt %d, want 2", plan2.attempts[0])
	}

	want := []string{
		"run_started",
		// The dead attempt is closed as aborted, naming itself as Next, before
		// the retry opens -- see internal/graph/dispatcher.go's heal. Without
		// this record the journal would hold two open node_started entries and
		// the next journal.Open would refuse the file forever after.
		"plan#1 started", "plan#1 finished aborted -> plan",
		"plan#2 started", "plan#2 finished ok -> code",
		"code#1 started", "code#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(); !durabilityEqualStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDurability_CrashBetweenPatchAndJournal_IsNotDoubleApplied is acceptance
// scenario 2: the crash window the dispatcher's write ordering deliberately
// makes reachable. Store.ApplyPatch already landed on state.json -- including
// the History entry settle() writes as part of that same patch -- but the
// process died before node_finished was journalled.
//
// This exact interleaving cannot be produced by running the real dispatcher
// and injecting a panic: the gap between ApplyPatch and the journal Append
// lives entirely inside the unexported settle method, one write wide, with
// no hook a test outside the package can land a panic in. So this test
// reconstructs the disk state precisely instead, the same way a debugger
// attached to the corpse of a crashed process would read it back.
//
// Resuming re-runs the node once more. Because every Patch field but History
// is an absolute set (see state.Patch's doc comment), re-applying the same
// values a second time lands on exactly the same State as applying them
// once: a slice field (Code.ChangedFiles here) ends at its usual length, not
// double it.
func TestDurability_CrashBetweenPatchAndJournal_IsNotDoubleApplied(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "no double apply")

	pre := h.state()
	pre.Code = state.Code{SessionID: "sess-1", LastDiff: "diff-0001.patch", ChangedFiles: []string{"a.go", "b.go"}}
	// settle() always attaches a History entry to the very same Patch it
	// applies, keyed at the crashed attempt's own node_started Seq (2 here:
	// 1 is run_started). Reconstructing it is part of reconstructing the
	// crash faithfully.
	pre.History = append(pre.History, state.HistoryEntry{
		Seq: 2, Node: graph.NodeCode, Attempt: 1, Status: "ok", Next: graph.End,
		Summary: "applied just before the process died; the journal never heard about it",
	})
	h.saveState(pre)
	h.seedJournal(
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
	)

	// The re-run returns the exact same absolute patch the crashed attempt
	// did -- a real node re-computing its own output is expected to be
	// deterministic given the same inputs, which is exactly what State is.
	code := &durabilityNode{name: graph.NodeCode, steps: []durabilityStep{{
		res: graph.Result{Next: graph.End, Status: journal.StatusOK, Patch: state.Patch{
			Code: &state.Code{SessionID: "sess-1", LastDiff: "diff-0001.patch", ChangedFiles: []string{"a.go", "b.go"}},
		}},
	}}}

	out, err := h.dispatcher(code).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}
	if code.calls != 1 || code.attempts[0] != 2 {
		t.Fatalf("code ran %d times at attempts %v, want 1 call at attempt 2", code.calls, code.attempts)
	}
	// The re-run observed the state its crashed attempt had already written:
	// re-running is safe precisely because it is re-computing from durable
	// truth, not from a blank slate.
	if code.seenCode[0].SessionID != "sess-1" || len(code.seenCode[0].ChangedFiles) != 2 {
		t.Errorf("re-run observed Code = %+v, want the crashed attempt's already-durable patch", code.seenCode[0])
	}

	got := h.state().Code
	if len(got.ChangedFiles) != 2 {
		t.Errorf("Code.ChangedFiles has %d entries after re-applying the same absolute patch, want 2 (not doubled)", len(got.ChangedFiles))
	}
	if diff := got.SessionID; diff != "sess-1" {
		t.Errorf("Code.SessionID = %q, want %q", diff, "sess-1")
	}

	// History's Seq-keyed dedup is what makes a repeated Apply of the very
	// same HistoryEntry safe. It cannot be exercised by this exact scenario,
	// though, because the retry always gets a fresh node_started Seq -- see
	// the doc comment on state.Patch.History and this test's own package
	// comment for the distinction. What must hold here is the weaker, more
	// important property: no two entries in the final History claim the same
	// Seq (the dedup key would silently merge them if they did, hiding a real
	// double-write), and there is no more than one entry per Seq actually
	// used.
	seen := map[uint64]int{}
	for _, e := range h.state().History {
		seen[e.Seq]++
	}
	for seq, n := range seen {
		if n > 1 {
			t.Errorf("State.History has %d entries at Seq %d, want at most 1: Seq is the idempotency key", n, seq)
		}
	}
}

// TestDurability_CrashBetweenPatchAndJournal_LeavesAStaleHistoryEntry is a
// characterization test for a real inconsistency this suite found while
// trying to break resume -- not one the acceptance criteria asked for.
//
// settle() attaches a HistoryEntry to the very same Patch it applies through
// Store.ApplyPatch, BEFORE the node_finished record that is supposed to
// describe that same execution is journalled (the crash window
// TestDurability_CrashBetweenPatchAndJournal_IsNotDoubleApplied reconstructs
// above). If the process dies in that window, state.json already durably
// holds a HistoryEntry claiming the crashed attempt finished "ok" -- before
// the journal has any opinion at all. dispatcher.heal only repairs the
// journal: it closes the dead attempt there as "aborted" (see the journal
// assertion below), but it never revisits state.json, so the stale,
// contradictory entry survives resume untouched, sitting right alongside the
// successful retry's own, correct entry. state.Patch.History's Seq-keyed
// dedup does not catch this, because the crashed attempt and its retry each
// get their own node_started Seq -- they are never the same key.
//
// This does not corrupt the run itself: journal.ResolveStart never reads
// State.History, only the journal, so which node runs next -- and the
// budget ledger -- are unaffected. What it does corrupt is the blackboard's
// own historical record: `belay timeline`, or any future feature reading
// State.History directly instead of the journal, would show a phantom "ok"
// execution of code#1 that its own sibling journal record calls aborted.
func TestDurability_CrashBetweenPatchAndJournal_LeavesAStaleHistoryEntry(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "characterize a found inconsistency")

	pre := h.state()
	pre.History = append(pre.History, state.HistoryEntry{
		Seq: 2, Node: graph.NodeCode, Attempt: 1, Status: "ok", Next: graph.End,
		Summary: "the crashed attempt's own claim, written before the process died",
	})
	h.saveState(pre)
	h.seedJournal(
		journal.Record{Event: journal.EventRunStarted},
		journal.Record{Event: journal.EventNodeStarted, Node: graph.NodeCode, Attempt: 1},
	)

	code := durabilityOK(graph.NodeCode, graph.End)
	if _, err := h.dispatcher(code).Resume(context.Background()); err != nil {
		t.Fatalf("Resume() error = %v, want nil", err)
	}

	hist := h.state().History
	if len(hist) != 2 {
		t.Fatalf("State.History has %d entries, want 2 (this test pins a found inconsistency; "+
			"if the count changed, check whether it was fixed or just moved): %+v", len(hist), hist)
	}
	stale, retried := hist[0], hist[1]
	if stale.Seq != 2 || stale.Attempt != 1 || stale.Status != "ok" {
		t.Fatalf("hist[0] = %+v, want the crashed attempt's stale claim (Seq 2, attempt 1, status ok)", stale)
	}
	if retried.Attempt != 2 || retried.Status != "ok" {
		t.Fatalf("hist[1] = %+v, want the successful retry (attempt 2, status ok)", retried)
	}

	// The journal -- the actual source of truth journal.ResolveStart uses --
	// correctly disagrees with the stale entry: it recorded that same attempt
	// as aborted.
	var journalStatus journal.Status
	for _, r := range h.readJournal().Records {
		if r.Event == journal.EventNodeFinished && r.Node == graph.NodeCode && r.Attempt == 1 {
			journalStatus = r.Status
		}
	}
	if journalStatus != journal.StatusAborted {
		t.Fatalf("journal's own record of code#1 = %v, want aborted", journalStatus)
	}
	if stale.Status == journalStatus.String() {
		t.Fatalf("the stale History entry now agrees with the journal (%q); "+
			"this test no longer characterizes the inconsistency it was written to pin", journalStatus)
	}
}

// TestDurability_WriteOrderingFailureAfterTheFirstWriteNeverSkipsANode proves
// the ordering contract in internal/graph/dispatcher.go's doc comment live,
// against the real Dispatcher, rather than by reconstructing its output by
// hand as the two tests above do.
//
// The gap between Store.ApplyPatch and the journal's node_finished Append
// lives inside an unexported method, one write wide, with no hook a
// black-box test can land a panic in -- so this test does not try to crash
// the process there. Instead it injects a real, deterministic write failure
// exactly at that boundary, using ordinary filesystem permissions: the run
// directory is made read-only after the journal file already exists (so
// appending to its already-open descriptor needs no directory permission at
// all) but before the node runs, so state.json's atomic write -- which
// creates a fresh temp file in that same directory on every call -- is the
// one call this can reliably fail, whichever of the two writes the
// dispatcher happens to perform second.
//
// If the dispatcher applies the patch first (what it actually does), the
// failing write is ApplyPatch itself: nothing lands durably, and the run is
// left exactly as if it had crashed inside the node, safe to retry. If the
// two writes were ever reordered, the failing write would be ApplyPatch
// AFTER the journal's node_finished had already durably landed -- the exact
// "durable record, no matching effect" defect the doc comment warns about.
// This test asserts that never happens, and this suite's mutation test
// swaps the order to confirm this assertion actually notices.
func TestDurability_WriteOrderingFailureAfterTheFirstWriteNeverSkipsANode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses the directory permissions this test depends on")
	}

	h := newDurabilityRun(t, config.Default(), "prove the write ordering live")
	// Seeding only run_started means journal.ResolveStart resolves to
	// journal.FirstNode ("plan"): the node under test must be named to match,
	// so this test exercises the very first node a run would actually
	// dispatch, not a hand-picked name the registry would reject as unknown.
	h.seedJournal(journal.Record{Event: journal.EventRunStarted})

	plan := &durabilityNode{name: graph.NodePlan, steps: []durabilityStep{{
		res: graph.Result{Next: graph.End, Status: journal.StatusOK,
			Patch: state.Patch{Code: &state.Code{SessionID: "should-never-land-without-its-journal-record"}}},
	}}}
	d := h.dispatcher(plan)

	// #nosec G302 -- this chmods a directory, not a data file: it needs the
	// execute bit to stay traversable, and the whole point of this test is
	// removing exactly the write bit that atomicWriteFile's temp-file
	// creation depends on.
	if err := os.Chmod(h.layout.RunDir(), 0o500); err != nil {
		t.Fatalf("Chmod(run dir, 0500) error = %v", err)
	}
	_, runErr := d.Run(context.Background())
	// #nosec G302 -- restoring the run directory's normal permissions (see
	// state.dirPerm), not a data file.
	if err := os.Chmod(h.layout.RunDir(), 0o750); err != nil {
		t.Fatalf("Chmod(run dir, 0750) restore error = %v", err)
	}
	if runErr == nil {
		t.Fatal("Run() succeeded despite a read-only run directory; the fault injection did not take effect")
	}

	recs := h.readJournal().Records
	lastIsFinished := len(recs) > 0 && recs[len(recs)-1].Event == journal.EventNodeFinished
	patchLanded := h.state().Code.SessionID == "should-never-land-without-its-journal-record"
	if lastIsFinished && !patchLanded {
		t.Fatal("the journal durably recorded code#1 finished -> END while its patch never reached " +
			"state.json: this is exactly the 'silently skip a node' defect the write ordering exists " +
			"to prevent")
	}

	// Whatever happened, a fresh process must still recover cleanly.
	plan2 := durabilityOK(graph.NodePlan, graph.End)
	out, err := h.dispatcher(plan2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() to recover from the fault error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}
	if plan2.calls != 1 {
		t.Errorf("plan ran %d times recovering from the fault, want exactly 1: a failed write must not "+
			"have counted as done", plan2.calls)
	}
}

// TestDurability_RecoveryWorksTwiceInARow is acceptance scenario 3, and the
// regression test for the defect that made crash recovery work exactly once:
// retrying a crashed node used to append a second node_started while the
// dead attempt's own was still open, which journal.Validate correctly
// classifies as corruption -- and journal.Open refuses to append onto a
// corrupt journal ever again. The dispatcher's heal step (see
// dispatcher.go) closes the dead attempt first, specifically so this
// does not happen. This test crashes twice, resumes twice, and only then
// lets the run finish.
func TestDurability_RecoveryWorksTwiceInARow(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "crash twice")

	// Process 1: plan succeeds, code crashes.
	plan1 := durabilityOK(graph.NodePlan, graph.NodeCode)
	code1 := &durabilityNode{name: graph.NodeCode, steps: []durabilityStep{{crash: true}}}
	d1 := h.dispatcher(plan1, code1)
	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d1.Run(context.Background()) })

	if err := journal.Validate(h.readJournal().Records); err != nil {
		t.Fatalf("journal.Validate() after the first crash = %v, want nil", err)
	}

	// Process 2: the first recovery. code runs (attempt 2) and succeeds,
	// routing to test -- which then crashes in its turn.
	plan2 := durabilityOK(graph.NodePlan, graph.NodeCode) // must not run again
	code2 := durabilityOK(graph.NodeCode, graph.NodeTest)
	testNode2 := &durabilityNode{name: graph.NodeTest, steps: []durabilityStep{{crash: true}}}
	d2 := h.dispatcher(plan2, code2, testNode2)
	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d2.Run(context.Background()) })

	if plan2.calls != 0 {
		t.Fatalf("plan ran again during the first recovery, want 0: it already finished before any crash")
	}
	if code2.calls != 1 || code2.attempts[0] != 2 {
		t.Fatalf("code ran %d times at attempts %v during the first recovery, want 1 call at attempt 2", code2.calls, code2.attempts)
	}

	// The journal must still be valid AND still openable after one recovery
	// closed one dead attempt and opened (and then lost) another. This is
	// exactly the file the historical bug would have bricked.
	if err := journal.Validate(h.readJournal().Records); err != nil {
		t.Fatalf("journal.Validate() after the second crash = %v, want nil: a healed journal must stay valid", err)
	}
	j, err := journal.Open(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.Open() after two crashes = %v, want nil: the historical bug refused to open this file a second time", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal.Close() error = %v", err)
	}

	// Process 3: the second recovery, this time all the way to completion.
	plan3 := durabilityOK(graph.NodePlan, graph.NodeCode)
	code3 := durabilityOK(graph.NodeCode, graph.NodeTest)
	testNode3 := durabilityOK(graph.NodeTest, graph.End)
	out, err := h.dispatcher(plan3, code3, testNode3).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() during the second recovery error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}
	if plan3.calls != 0 || code3.calls != 0 {
		t.Fatalf("plan ran %d times, code ran %d times during the second recovery, want 0 for both: both already finished", plan3.calls, code3.calls)
	}
	if testNode3.calls != 1 || testNode3.attempts[0] != 2 {
		t.Fatalf("test ran %d times at attempts %v during the second recovery, want 1 call at attempt 2", testNode3.calls, testNode3.attempts)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished aborted -> code",
		"code#2 started", "code#2 finished ok -> test",
		"test#1 started", "test#1 finished aborted -> test",
		"test#2 started", "test#2 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(); !durabilityEqualStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDurability_TornJournalTail_IsHealedNotTreatedAsCorruption is acceptance
// scenario 4. journal.Append fsyncs every record individually, so a process
// killed mid-write can only ever leave the FINAL line of the file torn --
// never an earlier one. This test builds a real journal through real
// journal.Append calls (via a genuine crash, exactly as scenario 1 does),
// then physically truncates the file to simulate the kill landing mid-write
// of the very next record, and asserts the result is healed rather than
// refused.
func TestDurability_TornJournalTail_IsHealedNotTreatedAsCorruption(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "tolerate a torn write")

	plan := durabilityOK(graph.NodePlan, graph.NodeCode)
	code := &durabilityNode{name: graph.NodeCode, steps: []durabilityStep{{crash: true}}}
	d1 := h.dispatcher(plan, code)
	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d1.Run(context.Background()) })

	before := h.readJournal()
	if before.Incomplete {
		t.Fatalf("journal reports incomplete before this test tears anything")
	}
	if got := len(before.Records); got != 4 {
		t.Fatalf("journal has %d records before tearing, want 4 (run_started, plan started+finished, code started): %+v",
			got, before.Records)
	}

	// Physically tear the file: chop bytes off the tail of its last line,
	// exactly the shape a kill mid-write (or mid-fsync) of that one record
	// leaves on disk. The three earlier records, each already fsynced by a
	// prior, successful Append call, are untouched.
	raw, err := os.ReadFile(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("ReadFile(journal) error = %v", err)
	}
	const tornBytes = 12
	if len(raw) <= tornBytes {
		t.Fatalf("journal is only %d bytes, too small for this test to tear %d bytes off safely", len(raw), tornBytes)
	}
	torn := raw[:len(raw)-tornBytes]
	// #nosec G703 -- JournalPath is derived from state.Layout, which this
	// harness built from a t.TempDir() run directory this process created
	// for itself; there is no external input in this path.
	if err := os.WriteFile(h.layout.JournalPath(), torn, 0o600); err != nil {
		t.Fatalf("WriteFile(torn journal) error = %v", err)
	}

	afterTear, err := journal.ReadFile(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.ReadFile() after tearing = %v, want nil: a torn tail must not be a read error", err)
	}
	if !afterTear.Incomplete {
		t.Fatalf("ReadResult.Incomplete = false after truncating the last line, want true")
	}
	if got := len(afterTear.Records); got != 3 {
		t.Fatalf("ReadResult.Records has %d entries after tearing, want 3: every complete record must still be read", got)
	}

	// journal.Open is what the dispatcher calls first on every resume. It
	// must heal the torn tail and report having done so, not refuse the file.
	j, err := journal.Open(h.layout.JournalPath())
	if err != nil {
		t.Fatalf("journal.Open() on a torn tail = %v, want nil: a torn tail must not be treated as corruption", err)
	}
	if !j.RecoveredTornTail() {
		t.Error("Journal.RecoveredTornTail() = false, want true: this is the operator-visible signal a torn write was recovered")
	}
	if err := j.Close(); err != nil {
		t.Fatalf("journal.Close() error = %v", err)
	}

	// The healing is a real, durable rewrite -- not merely tolerated in
	// memory. A second, independent read sees a clean file with no trace of
	// the torn line.
	healed := h.readJournal()
	if healed.Incomplete {
		t.Error("journal is still reported incomplete after Open healed it")
	}
	if got := len(healed.Records); got != 3 {
		t.Errorf("healed journal has %d records, want 3", got)
	}

	// Process 2: resume. code's torn node_started record was never durable,
	// so as far as the journal is concerned it never happened -- code must
	// run fresh, at attempt 1, never attempt 2. This is the precise contrast
	// with TestDurability_CrashInsideNode_ReRunsAtAttempt2, where a COMPLETE
	// (not torn) node_started is honored and retried at attempt+1.
	plan2 := durabilityOK(graph.NodePlan, graph.NodeCode)
	code2 := durabilityOK(graph.NodeCode, graph.End)
	out, err := h.dispatcher(plan2, code2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() after healing a torn tail error = %v, want nil", err)
	}
	if out.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out.Status)
	}
	if plan2.calls != 0 {
		t.Errorf("plan ran again after its finish was already durably journalled, want 0 calls")
	}
	if code2.calls != 1 || code2.attempts[0] != 1 {
		t.Errorf("code ran %d times at attempts %v, want exactly 1 call at attempt 1 (its torn start was never durable)",
			code2.calls, code2.attempts)
	}

	want := []string{
		"run_started",
		"plan#1 started", "plan#1 finished ok -> code",
		"code#1 started", "code#1 finished ok -> END",
		"run_completed",
	}
	if got := h.timeline(); !durabilityEqualStrings(got, want) {
		t.Errorf("timeline =\n  %v\nwant\n  %v", got, want)
	}
}

// TestDurability_LedgerContinuesAcrossACrash_NotResets is acceptance
// scenario 5: budget.Restore, not budget.New, is what a resume must call, or
// every crash hands a runaway run a fresh ceiling to spend through again.
// This test spends real (fake) money before a crash and real money after
// one, and checks the two amounts landed on disk summed, exactly -- a
// numeric assertion, not merely "spend is non-zero".
func TestDurability_LedgerContinuesAcrossACrash_NotResets(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "keep counting spend")

	plan := &durabilityNode{name: graph.NodePlan, steps: []durabilityStep{{
		res: graph.Result{Next: graph.NodeCode, Status: journal.StatusOK,
			Usage: belay.Usage{InputTokens: 1000, OutputTokens: 200, USD: 1.50}},
	}}}
	code := &durabilityNode{name: graph.NodeCode, steps: []durabilityStep{{crash: true}}}
	d1 := h.dispatcher(plan, code)
	durabilityRunExpectingCrash(t, func() (graph.Outcome, error) { return d1.Run(context.Background()) })

	afterCrash := h.manifest().Budget
	if !durabilityCloseTo(afterCrash.SpentUSD, 1.50) {
		t.Fatalf("manifest.Budget.SpentUSD after the crash = %v, want 1.50 (plan's own spend, "+
			"persisted before code ever started)", afterCrash.SpentUSD)
	}

	plan2 := durabilityOK(graph.NodePlan, graph.NodeCode) // must not run again
	code2 := &durabilityNode{name: graph.NodeCode, steps: []durabilityStep{{
		res: graph.Result{Next: graph.End, Status: journal.StatusOK,
			Usage: belay.Usage{InputTokens: 500, OutputTokens: 100, USD: 2.25}},
	}}}
	out, err := h.dispatcher(plan2, code2).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if plan2.calls != 0 {
		t.Errorf("plan ran again after resume, want 0 calls: it already finished before the crash")
	}

	// 1.50 (before the crash, restored) + 2.25 (after) = 3.75, exactly. Not
	// 2.25, which is what a resume that called budget.New instead of
	// budget.Restore would report -- see this suite's mutation test.
	const want = 1.50 + 2.25
	if !durabilityCloseTo(out.Usage.USD, want) {
		t.Errorf("Outcome.Usage.USD = %v, want %v", out.Usage.USD, want)
	}
	got := h.manifest().Budget
	if !durabilityCloseTo(got.SpentUSD, want) {
		t.Errorf("manifest.Budget.SpentUSD = %v, want %v", got.SpentUSD, want)
	}
	if got.TokensIn != 1500 || got.TokensOut != 300 {
		t.Errorf("manifest.Budget tokens = in %d / out %d, want in 1500 / out 300", got.TokensIn, got.TokensOut)
	}
}

// TestDurability_PausedRunRefusesPlainRun_ThenAdvancesUnderResume is
// acceptance scenario 6. A paused run is a request for a human, not a crash
// -- but the CLI-facing contract (a distinct, non-zero exit code, and zero
// work performed on the refused call) matters just as much to a caller
// automating around belay as crash recovery does, and it shares the same
// journal machinery this suite otherwise tests.
func TestDurability_PausedRunRefusesPlainRun_ThenAdvancesUnderResume(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "await approval")

	plan := durabilityOK(graph.NodePlan, graph.NodeApprove)
	approve := &durabilityNode{name: graph.NodeApprove, steps: []durabilityStep{
		{res: graph.Result{Status: journal.StatusPaused, Note: "needs a human"}},
	}}
	out, err := h.dispatcher(plan, approve).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (pausing is a successful outcome)", err)
	}
	if out.Status != state.RunStatusPaused || out.ExitCode != graph.ExitOK {
		t.Fatalf("Outcome = {%v, exit %d}, want {paused, exit %d}", out.Status, out.ExitCode, graph.ExitOK)
	}

	// Process 2: a brand-new `belay run`, exactly what an operator who forgot
	// they left a run paused would type.
	plan2 := durabilityOK(graph.NodePlan, graph.NodeApprove)
	approve2 := &durabilityNode{name: graph.NodeApprove, steps: []durabilityStep{
		{res: graph.Result{Status: journal.StatusPaused, Note: "needs a human"}},
	}}
	out2, err2 := h.dispatcher(plan2, approve2).Run(context.Background())
	if !errors.Is(err2, graph.ErrRunPaused) {
		t.Fatalf("Run() on a paused run error = %v, want one wrapping ErrRunPaused", err2)
	}
	// The literal, user-visible number: the CLI maps this straight onto
	// os.Exit, and other tooling scripts against it, so this is asserted as
	// the raw int belay's own doc comment promises it will never be
	// renumbered to, not merely as the graph.ExitPaused symbol.
	if out2.ExitCode != 4 {
		t.Errorf("Outcome.ExitCode = %d, want the literal exit code 4", out2.ExitCode)
	}
	if plan2.calls != 0 || approve2.calls != 0 {
		t.Errorf("plan ran %d times, approve ran %d times; a refused Run must execute nothing",
			plan2.calls, approve2.calls)
	}

	// Process 3: `belay resume` advances it.
	plan3 := durabilityOK(graph.NodePlan, graph.NodeApprove)
	approve3 := &durabilityNode{name: graph.NodeApprove, steps: []durabilityStep{
		{res: graph.Result{Next: graph.End, Status: journal.StatusOK}},
	}}
	out3, err3 := h.dispatcher(plan3, approve3).Resume(context.Background())
	if err3 != nil {
		t.Fatalf("Resume() error = %v, want nil", err3)
	}
	if out3.Status != state.RunStatusCompleted {
		t.Fatalf("Outcome.Status = %v, want completed", out3.Status)
	}
	if plan3.calls != 0 {
		t.Errorf("plan ran again on resume, want 0: it already finished before the pause")
	}
	if approve3.calls != 1 {
		t.Errorf("approve ran %d times on resume, want 1", approve3.calls)
	}
}

// TestDurability_CompletedRunRefusesResume_DistinguishablyNotAnError is
// acceptance scenario 7. Resuming a completed run is a no-op success, not an
// error -- and that is exactly how a caller must tell it apart from a run
// that genuinely still had work to do: not by err != nil (a retry loop that
// only checks the error would wrongly conclude a second Resume did
// something), but by Outcome.Status, Outcome.ExitCode and Outcome.Note.
func TestDurability_CompletedRunRefusesResume_DistinguishablyNotAnError(t *testing.T) {
	h := newDurabilityRun(t, config.Default(), "already done")

	plan := durabilityOK(graph.NodePlan, graph.End)
	if _, err := h.dispatcher(plan).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	plan2 := durabilityOK(graph.NodePlan, graph.End)
	out, err := h.dispatcher(plan2).Resume(context.Background())
	if err != nil {
		t.Fatalf("Resume() on a completed run error = %v, want nil: completion is reported "+
			"through Outcome, not as an error", err)
	}
	if out.Status != state.RunStatusCompleted || out.ExitCode != graph.ExitOK {
		t.Errorf("Outcome = {%v, exit %d}, want {completed, exit %d}", out.Status, out.ExitCode, graph.ExitOK)
	}
	if out.Note == "" {
		t.Error("Outcome.Note is empty, want a note a caller can distinguish this no-op by")
	}
	if plan2.calls != 0 {
		t.Errorf("plan ran %d times resuming an already-completed run, want 0", plan2.calls)
	}
}

// TestDurability_NothingButDispatcherWritesControlState is acceptance
// scenario 8: the end-to-end pin of ADR 0002. It has two parts. The first
// drives a full, multi-node run through durabilityGuardedNode, which checks
// that state.json, manifest.json and journal.ndjson never change during its
// own Run call -- the only window in which the node's own code, rather than
// the dispatcher's, could be responsible for such a change. The second part
// proves that check is not vacuous, by driving a node that deliberately
// breaks ADR 0002 and confirming the guard actually notices.
func TestDurability_NothingButDispatcherWritesControlState(t *testing.T) {
	t.Run("a full run never lets a node touch control state", func(t *testing.T) {
		h := newDurabilityRun(t, config.Default(), "pin ADR 0002")

		startFP, err := durabilityFingerprint(h.layout)
		if err != nil {
			t.Fatalf("durabilityFingerprint() error = %v", err)
		}

		plan := &durabilityGuardedNode{tb: t, name: graph.NodePlan, layout: h.layout,
			work: func(*graph.RunContext) graph.Result {
				return graph.Result{Next: graph.NodeCode, Status: journal.StatusOK}
			}}
		code := &durabilityGuardedNode{tb: t, name: graph.NodeCode, layout: h.layout,
			work: func(*graph.RunContext) graph.Result {
				return graph.Result{Next: graph.NodeTest, Status: journal.StatusOK,
					Patch: state.Patch{Code: &state.Code{ChangedFiles: []string{"a.go"}}}}
			}}
		testNode := &durabilityGuardedNode{tb: t, name: graph.NodeTest, layout: h.layout,
			work: func(*graph.RunContext) graph.Result {
				return graph.Result{Next: graph.End, Status: journal.StatusOK}
			}}

		out, err := h.dispatcher(plan, code, testNode).Run(context.Background())
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if out.Status != state.RunStatusCompleted {
			t.Fatalf("Outcome.Status = %v, want completed", out.Status)
		}

		endFP, err := durabilityFingerprint(h.layout)
		if err != nil {
			t.Fatalf("durabilityFingerprint() error = %v", err)
		}
		if startFP == endFP {
			t.Fatal("control state's fingerprint never changed across the whole run; " +
				"the guard above would pass vacuously if the dispatcher itself had written nothing at all")
		}
	})

	t.Run("the guard fires on a node that violates ADR 0002", func(t *testing.T) {
		h := newDurabilityRun(t, config.Default(), "prove the guard is not vacuous")
		fake := &durabilityFakeT{}
		hostile := &durabilityGuardedNode{tb: fake, name: graph.NodePlan, layout: h.layout,
			work: func(*graph.RunContext) graph.Result {
				// Deliberately breaks ADR 0002: a node reaching past its own
				// Result to write manifest.json directly, the way a bug (not
				// a malicious node -- belay has no sandbox against its own
				// code) could.
				_ = os.WriteFile(h.layout.ManifestPath(), []byte(`{"tampered":true}`), 0o600)
				return graph.Result{Next: graph.End, Status: journal.StatusOK}
			}}

		_, _ = h.dispatcher(hostile).Run(context.Background())

		if len(fake.errors) == 0 {
			t.Fatal("the control-state guard did not fire for a node that wrote manifest.json directly " +
				"from inside its own Run call; the check in the sibling subtest would be vacuous")
		}
	})
}
