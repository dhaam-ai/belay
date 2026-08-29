package journal

import "testing"

// TestStatsCountsLoopIterations is the acceptance test for requirement 7:
// Stats must correctly count loop iterations for a run that visits test and
// fix repeatedly (test -> fix -> test -> fix -> test), not just distinct
// nodes.
func TestStatsCountsLoopIterations(t *testing.T) {
	t.Parallel()

	stats := Stats(buildSampleRun(t))

	byNode := make(map[string]NodeStats, len(stats))
	var order []string
	for _, s := range stats {
		byNode[s.Node] = s
		order = append(order, s.Node)
	}

	wantOrder := []string{"plan", "approve", "code", "write", "test", "fix", "review"}
	if len(order) != len(wantOrder) {
		t.Fatalf("Stats returned %d nodes %v, want %d nodes %v", len(order), order, len(wantOrder), wantOrder)
	}
	for i, node := range wantOrder {
		if order[i] != node {
			t.Errorf("Stats order[%d] = %q, want %q (nodes must appear in first-seen order)", i, order[i], node)
		}
	}

	test, ok := byNode["test"]
	if !ok {
		t.Fatal(`Stats has no entry for "test"`)
	}
	if test.Executions != 3 {
		t.Errorf(`test.Executions = %d, want 3 (test -> fix -> test -> fix -> test visits "test" three times)`, test.Executions)
	}
	if want := int64(600); test.TotalDuration.Milliseconds() != want {
		t.Errorf("test.TotalDuration = %v, want %dms (100+200+300)", test.TotalDuration, want)
	}
	if got := test.ByStatus[StatusFailed]; got != 2 {
		t.Errorf("test.ByStatus[StatusFailed] = %d, want 2", got)
	}
	if got := test.ByStatus[StatusOK]; got != 1 {
		t.Errorf("test.ByStatus[StatusOK] = %d, want 1", got)
	}

	fix, ok := byNode["fix"]
	if !ok {
		t.Fatal(`Stats has no entry for "fix"`)
	}
	if fix.Executions != 2 {
		t.Errorf(`fix.Executions = %d, want 2`, fix.Executions)
	}
	if want := int64(110); fix.TotalDuration.Milliseconds() != want {
		t.Errorf("fix.TotalDuration = %v, want %dms (50+60)", fix.TotalDuration, want)
	}
	if got := fix.ByStatus[StatusOK]; got != 2 {
		t.Errorf("fix.ByStatus[StatusOK] = %d, want 2", got)
	}

	for _, node := range []string{"plan", "approve", "code", "write", "review"} {
		s := byNode[node]
		if s.Executions != 1 {
			t.Errorf("%s.Executions = %d, want 1", node, s.Executions)
		}
	}
}

func TestStatsOnEmptyOrNilIsEmpty(t *testing.T) {
	t.Parallel()

	for _, records := range [][]Record{nil, {}} {
		if got := Stats(records); len(got) != 0 {
			t.Errorf("Stats(%v) = %v, want empty", records, got)
		}
	}
}

// TestStatsCountsOnlyFinishedExecutions proves Stats counts completed
// executions (node_finished), not attempts: an orphaned node_started (a
// node that died mid-execution and was never closed) contributes nothing
// until it is retried and actually finishes.
func TestStatsCountsOnlyFinishedExecutions(t *testing.T) {
	t.Parallel()

	records := []Record{
		{Seq: 1, Event: EventRunStarted},
		{Seq: 2, Node: "plan", Attempt: 1, Event: EventNodeStarted},
		{Seq: 3, Event: EventRunPaused},
	}

	stats := Stats(records)
	if len(stats) != 0 {
		t.Errorf("Stats on a run with no node_finished records = %v, want empty", stats)
	}
}
