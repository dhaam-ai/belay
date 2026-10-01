//go:build unix

package fork_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/fork"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// TestPrepareChoosesTheVisit: --visit counts visits as journal.Visits does,
// which is how `belay timeline` numbers them, and no number means the last.
// The files are accepted as they are, so only the choice of visit is tested.
func TestPrepareChoosesTheVisit(t *testing.T) {
	src := newSource(t)

	tests := []struct {
		name        string
		node        string
		visit       int
		wantOrdinal int
		wantVisits  int
		wantErr     error
		wantSaying  string // a phrase the refusal must contain
	}{
		{name: "no visit named means the last", node: graph.NodeFix, wantOrdinal: 2, wantVisits: 2},
		{name: "the first visit", node: graph.NodeFix, visit: 1, wantOrdinal: 1, wantVisits: 2},
		{name: "a middle visit", node: graph.NodeTest, visit: 2, wantOrdinal: 2, wantVisits: 3},
		{name: "a node visited once", node: graph.NodePlan, wantOrdinal: 1, wantVisits: 1},
		{
			name: "past the last visit", node: graph.NodeTest, visit: 4,
			wantErr: fork.ErrNoSuchVisit, wantSaying: "ran test 3 times",
		},
		{
			name: "a negative visit", node: graph.NodeTest, visit: -1,
			wantErr: fork.ErrNoSuchVisit, wantSaying: "ran test 3 times",
		},
		{
			name: "a node the run never reached", node: graph.NodePolicy,
			wantErr: fork.ErrNoSuchVisit, wantSaying: "plan, approve, code, test, fix, review",
		},
		{name: "fanout", node: graph.NodeFanout, wantErr: fork.ErrUnsupportedNode, wantSaying: "candidate"},
		{name: "join", node: graph.NodeJoin, wantErr: fork.ErrUnsupportedNode, wantSaying: "candidate"},
		{name: "promote", node: graph.NodePromote, wantErr: fork.ErrUnsupportedNode, wantSaying: "commit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := src.request(tt.node, tt.visit)
			req.AcceptCurrentFiles = true

			spec, err := fork.Prepare(context.Background(), req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Prepare() error = %v, want %v", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantSaying) {
					t.Errorf("Prepare() error = %q, want it to say %q", err, tt.wantSaying)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if spec.Visit.Node != tt.node || spec.Visit.Ordinal != tt.wantOrdinal || spec.Visits != tt.wantVisits {
				t.Errorf("chose %s #%d of %d, want %s #%d of %d",
					spec.Visit.Node, spec.Visit.Ordinal, spec.Visits, tt.node, tt.wantOrdinal, tt.wantVisits)
			}
			if want := src.visit(t, tt.node, tt.wantOrdinal); spec.Visit.StartSeq != want.StartSeq {
				t.Errorf("Visit.StartSeq = %d, want %d", spec.Visit.StartSeq, want.StartSeq)
			}
			if spec.Snapshot.Seq != spec.Visit.StartSeq || spec.Snapshot.Node != tt.node {
				t.Errorf("snapshot is of %s at seq %d, want the visit's own, at seq %d",
					spec.Snapshot.Node, spec.Snapshot.Seq, spec.Visit.StartSeq)
			}
		})
	}
}

// TestPrepareRefusesASourceThatMayStillBeRunning: a run whose manifest says
// running is either being driven right now or died part-way, and a torn
// journal tail is a write that was cut off. Either way a step may still be
// changing the files a fork would copy. Every run that has stopped is fair
// game, including one paused or one that failed.
func TestPrepareRefusesASourceThatMayStillBeRunning(t *testing.T) {
	tests := []struct {
		name    string
		status  state.RunStatus
		torn    bool
		wantErr error
	}{
		{name: "running", status: state.RunStatusRunning, wantErr: fork.ErrSourceActive},
		{name: "a torn journal", status: state.RunStatusCompleted, torn: true, wantErr: fork.ErrSourceActive},
		{name: "completed", status: state.RunStatusCompleted},
		{name: "paused", status: state.RunStatusPaused},
		{name: "failed", status: state.RunStatusFailed},
		{name: "aborted", status: state.RunStatusAborted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			src.setManifest(t, func(m *state.Manifest) { m.Status = tt.status })
			if tt.torn {
				f, err := os.OpenFile(src.layout.JournalPath(), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatalf("OpenFile: %v", err)
				}
				if _, err := f.WriteString(`{"seq":21,"event":"run_st`); err != nil {
					t.Fatalf("WriteString: %v", err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}

			_, err := fork.Prepare(context.Background(), src.request(graph.NodeTest, 0))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Prepare() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestPrepareRefusesAVisitWithNoSnapshot: a run recorded before snapshots
// existed cannot be forked, and there is no falling back on state.json,
// which holds the blackboard as the run left it, after the visit.
func TestPrepareRefusesAVisitWithNoSnapshot(t *testing.T) {
	src := newSource(t)
	if err := os.RemoveAll(src.layout.SnapshotsDir()); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	_, err := fork.Prepare(context.Background(), src.request(graph.NodeTest, 0))
	if !errors.Is(err, fork.ErrNoSnapshot) {
		t.Fatalf("Prepare() error = %v, want ErrNoSnapshot", err)
	}
	if !strings.Contains(err.Error(), "state.json") {
		t.Errorf("Prepare() error = %q, want it to say why state.json will not do", err)
	}
}

// TestPrepareComparesTheFiles: the files are the one input belay cannot put
// back as they were, so a fork asks before running a node on files that are
// not the ones it saw. A fingerprint belay did not record, or recorded by a
// scheme it cannot compare, is no evidence that nothing changed.
func TestPrepareComparesTheFiles(t *testing.T) {
	tests := []struct {
		name        string
		node        string
		visit       int
		accept      bool
		digest      func(recorded string) string // rewrites the visit's recorded digest; nil leaves it
		wantErr     error
		wantChanged bool
		wantSaying  string // in the refusal, or in a deviation when accepted
	}{
		{name: "unchanged since the visit", node: graph.NodeTest, visit: 3},
		{
			name: "changed since the visit", node: graph.NodeFix, visit: 2,
			wantErr: fork.ErrFilesChanged, wantSaying: "have changed since fix #2 ran",
		},
		{
			name: "changed, and accepted", node: graph.NodeFix, visit: 2, accept: true,
			wantChanged: true, wantSaying: "as they are now",
		},
		{
			name: "no fingerprint was recorded", node: graph.NodeTest, visit: 3,
			digest:  func(string) string { return "" },
			wantErr: fork.ErrFilesChanged, wantSaying: "recorded no fingerprint",
		},
		{
			name: "recorded by another scheme", node: graph.NodeTest, visit: 3,
			digest:  func(d string) string { return "tree9:sha256:" + d[strings.LastIndex(d, ":")+1:] },
			wantErr: fork.ErrFilesChanged, wantSaying: "cannot be compared",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			if tt.digest != nil {
				src.rewriteSnapshot(t, tt.node, tt.visit, func(s *state.Snapshot) { s.WorkspaceDigest = tt.digest(s.WorkspaceDigest) })
			}
			req := src.request(tt.node, tt.visit)
			req.AcceptCurrentFiles = tt.accept

			spec, err := fork.Prepare(context.Background(), req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Prepare() error = %v, want %v", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantSaying) {
					t.Errorf("Prepare() error = %q, want it to say %q", err, tt.wantSaying)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if spec.FilesChanged != tt.wantChanged {
				t.Errorf("FilesChanged = %v, want %v", spec.FilesChanged, tt.wantChanged)
			}
			current, err := survey(context.Background(), src.ws)
			if err != nil {
				t.Fatalf("survey: %v", err)
			}
			if spec.Digest != current {
				t.Errorf("Digest = %q, want the tree's fingerprint as it is now, %q", spec.Digest, current)
			}
			if tt.wantSaying != "" && !strings.Contains(strings.Join(spec.Deviations, "\n"), tt.wantSaying) {
				t.Errorf("Deviations = %q, want one saying %q", spec.Deviations, tt.wantSaying)
			}
		})
	}
}

// TestPrepareHoldsTheNodesThatReadThePlanToIt: code takes the approved plan
// as its instructions, so it is refused a plan that is not the one the gate
// approved. The plan node and the gate judge the plan themselves, so a
// changed one is something to say, not a refusal. Nothing after code reads
// the plan at all.
func TestPrepareHoldsTheNodesThatReadThePlanToIt(t *testing.T) {
	tests := []struct {
		name       string
		node       string
		plan       *string // the plan as it is now; nil when it cannot be found
		wantErr    error
		wantSaying string // in the refusal, or in a deviation
	}{
		{name: "code, plan as approved", node: graph.NodeCode, plan: ptr(planText)},
		{
			name: "code, plan edited since", node: graph.NodeCode, plan: ptr(planText + "2. And more.\n"),
			wantErr: fork.ErrPlanChanged, wantSaying: "approved",
		},
		{
			name: "approve, plan edited since", node: graph.NodeApprove, plan: ptr(planText + "2. And more.\n"),
			wantSaying: "the plan has changed",
		},
		{name: "code, plan gone", node: graph.NodeCode, wantErr: fork.ErrNoPlan},
		{name: "approve, plan gone", node: graph.NodeApprove, wantErr: fork.ErrNoPlan},
		{name: "test, plan gone", node: graph.NodeTest, wantSaying: "nothing it re-runs reads it"},
		{name: "test, plan edited since", node: graph.NodeTest, plan: ptr("something else\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			req := src.request(tt.node, 0)
			req.AcceptCurrentFiles = true
			req.Plan = func() ([]byte, error) {
				if tt.plan == nil {
					return nil, os.ErrNotExist
				}
				return []byte(*tt.plan), nil
			}

			spec, err := fork.Prepare(context.Background(), req)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Prepare() error = %v, want %v", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantSaying) {
					t.Errorf("Prepare() error = %q, want it to say %q", err, tt.wantSaying)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			deviations := strings.Join(spec.Deviations, "\n")
			if tt.wantSaying != "" && !strings.Contains(deviations, tt.wantSaying) {
				t.Errorf("Deviations = %q, want one saying %q", spec.Deviations, tt.wantSaying)
			}
			// Every deviation about the plan begins "the plan"; the word on
			// its own also appears in the test's temporary directory names.
			if tt.wantSaying == "" && strings.Contains(deviations, "the plan") {
				t.Errorf("Deviations = %q, want nothing about a plan the node does not read", spec.Deviations)
			}
		})
	}
}

// TestPrepareNamesEveryDeviation: before it spends anything, a fork says every
// way its inputs differ from what the visit was given, naming the setting and
// both values. Settings a fork never takes from belay.yaml, and the ones every
// fork changes, are not differences in what the node is given.
//
// Every visit after code holds the agent's conversation on its blackboard,
// and no fork carries it over, so every such row says so.
func TestPrepareNamesEveryDeviation(t *testing.T) {
	const conversation = "the agent's conversation"
	tests := []struct {
		name   string
		node   string
		change func(src *source, req *fork.Request)
		want   []string // phrases, each in exactly one deviation
	}{
		{
			name: "nothing differs", node: graph.NodeTest,
			change: func(src *source, _ *fork.Request) {
				src.rewriteSnapshot(t, graph.NodeTest, 3, func(s *state.Snapshot) { s.State.Code.SessionID = "" })
			},
		},
		{name: "the conversation", node: graph.NodeTest, want: []string{conversation}},
		{
			name: "the model, on files that have changed", node: graph.NodeFix,
			change: func(_ *source, req *fork.Request) { req.Config.Agent.Models = map[string]string{"fix": "opus"} },
			want:   []string{`fix is sent the model "opus"`, conversation, "as they are now"},
		},
		{
			name: "a setting", node: graph.NodeTest,
			change: func(_ *source, req *fork.Request) { req.Config.Graph.GiveUp = 7 },
			want:   []string{`graph.give_up: was "3", now "7"`, conversation},
		},
		{
			name: "settings a fork does not take from belay.yaml", node: graph.NodeTest,
			change: func(_ *source, req *fork.Request) {
				req.Config.Output.Mode = "in_place"
				req.Config.Plan.Archive = false
				req.Config.Policy.MaxChangedFiles = 3
				req.Config.Fanout.Candidates = 5
			},
			want: []string{conversation},
		},
		{
			name: "a source on a branch", node: graph.NodeTest,
			change: func(src *source, _ *fork.Request) {
				src.setManifest(t, func(m *state.Manifest) {
					m.SetOutput(state.Output{Mode: state.OutputBranch, Branch: "belay/" + sourceID})
				})
			},
			want: []string{"belay/" + sourceID, conversation},
		},
		{
			name: "a source that fanned out", node: graph.NodeTest,
			change: func(src *source, _ *fork.Request) {
				src.setManifest(t, func(m *state.Manifest) { m.SetFanout(state.FanoutPin{Candidates: 3, Select: "fewest_issues"}) })
			},
			want: []string{"3 candidates", conversation},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t)
			req := src.request(tt.node, 0)
			req.AcceptCurrentFiles = true
			if tt.change != nil {
				tt.change(src, &req)
			}

			spec, err := fork.Prepare(context.Background(), req)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if spec.Deviations == nil {
				t.Error("Deviations = nil, want an empty list for a fork that differs in nothing")
			}
			for _, phrase := range tt.want {
				n := 0
				for _, d := range spec.Deviations {
					if strings.Contains(d, phrase) {
						n++
					}
				}
				if n != 1 {
					t.Errorf("%d deviations say %q, want exactly 1; got %q", n, phrase, spec.Deviations)
				}
			}
			if len(spec.Deviations) != len(tt.want) {
				t.Errorf("Deviations = %q, want %d of them", spec.Deviations, len(tt.want))
			}
		})
	}
}

// TestPrepareInheritsTheHistoryWithoutItsSpend: the fork's journal begins
// with every record its source made before the visit, and stops short of
// the visit's own node_started, which the fork writes itself. Every dollar
// belongs to the run that spent it, so the inherited records carry no usage
// and the amount is reported beside them instead.
func TestPrepareInheritsTheHistoryWithoutItsSpend(t *testing.T) {
	src := newSource(t)
	req := src.request(graph.NodeFix, 2)
	req.AcceptCurrentFiles = true

	spec, err := fork.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	var want []journal.Record
	var paid float64
	for _, r := range src.records(t) {
		if r.Seq >= spec.Visit.StartSeq {
			break
		}
		if r.Usage != nil {
			paid += r.Usage.USD
			r.Usage = nil
		}
		want = append(want, r)
	}
	if diff := cmp.Diff(want, spec.Inherited); diff != "" {
		t.Errorf("Inherited (-want +got):\n%s", diff)
	}
	// plan, approve, code, test #1, fix #1 and test #2 ran before fix #2.
	if paid < 0.5999 || paid > 0.6001 {
		t.Fatalf("the source paid %v before fix #2, want 0.60", paid)
	}
	if spec.InheritedUSD != paid || spec.InheritedEstimated {
		t.Errorf("Inherited spend = %v (estimated %v), want %v, reported", spec.InheritedUSD, spec.InheritedEstimated, paid)
	}
	if got := filepath.Base(spec.Tree); got != filepath.Base(src.ws) {
		t.Errorf("Tree = %q, want the tree the source worked in, %q", spec.Tree, src.ws)
	}
}

func ptr(s string) *string { return &s }
