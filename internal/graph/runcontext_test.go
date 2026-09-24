package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/internal/vcs"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This is an internal test because runContext is the seam, not the API: it is
// the one place a node's RunContext is assembled, and the place later work
// adds fields. The dispatcher's black-box tests only ever read Goal, Step and
// Attempt back out of a node, so nothing else a node is handed was checked.

// The adapter fakes embed the interface they stand in for. runContext only
// passes them through, so no method is ever called; each is a distinct type
// so that handing one adapter to another's field cannot compare equal.
type (
	rcAgent    struct{ belay.AgentBackend }
	rcRunner   struct{ belay.TestRunner }
	rcLinter   struct{ belay.Linter }
	rcReviewer struct{ belay.Reviewer }
	rcIsolator struct{ belay.Isolator }
	rcVCS      struct{ vcs.Repository }
)

// rcIdle exists because NewDispatcher refuses an empty registry.
type rcIdle struct{}

func (rcIdle) Name() string { return NodeCode }
func (rcIdle) Run(context.Context, *RunContext) (Result, error) {
	return Result{Next: End, Status: journal.StatusOK}, nil
}

// TestRunContextGivesANodeTheConfigThisRunPinned covers the half of the pin
// that is not the registry. Wiring a run's nodes from the manifest keeps the
// graph's shape, but two nodes read configuration on every execution —
// fanout asks how many candidates there are, join asks which strategy picks
// the winner — and a RunContext filled from the file as this process loaded
// it would hand them whatever belay.yaml says at resume time.
func TestRunContextGivesANodeTheConfigThisRunPinned(t *testing.T) {
	const runID = "20260101T000000Z-0123456789ab"

	// Deliberately different from the pin in every field, and with limits
	// the run was not created with, which must still come through live.
	live := config.Default()
	live.Fanout.Enabled = false
	live.Fanout.Candidates = 5
	live.Fanout.Select = config.SelectFastest
	live.Budget.MaxUSD = 9.99
	live.Graph.MaxSteps = 42
	live.Graph.NodeTimeout = config.NewDuration(7 * time.Minute)

	pin := state.FanoutPin{Candidates: 3, Select: string(config.SelectFewestIssues)}

	tests := []struct {
		name  string
		pin   *state.FanoutPin
		want  config.Fanout
		limit float64
	}{
		{
			name: "a run created without fanout reads the configuration as it stands",
			want: live.Fanout,
		},
		{
			name: "a run that fans out reads the shape it was created with",
			pin:  &pin,
			want: config.Fanout{
				Enabled: true, Candidates: 3,
				Isolator: live.Fanout.Isolator, Select: config.SelectFewestIssues,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := t.TempDir()
			layout, err := state.NewLayout(ws, runID)
			if err != nil {
				t.Fatalf("NewLayout: %v", err)
			}
			reg := NewRegistry()
			reg.MustRegister(rcIdle{})
			d, err := NewDispatcher(Options{Store: state.NewStore(layout), Registry: reg, Config: live})
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}

			man := state.NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), runID, ws, "ship it", live)
			if tt.pin != nil {
				man.SetFanout(*tt.pin)
			}

			rc := d.runContext(&man, state.NewState("ship it"), budget.New(), NodeFanout, 1, 1)

			if !reflect.DeepEqual(rc.Config.Fanout, tt.want) {
				t.Errorf("RunContext.Config.Fanout = %+v, want %+v", rc.Config.Fanout, tt.want)
			}
			// The limits are deliberately NOT pinned: raising one and
			// resuming is what continues a run that met it, so a node must
			// see the figures as they are now.
			if rc.Config.Budget.MaxUSD != 9.99 {
				t.Errorf("RunContext.Config.Budget.MaxUSD = %v, want the live 9.99", rc.Config.Budget.MaxUSD)
			}
			if rc.Config.Graph.MaxSteps != 42 {
				t.Errorf("RunContext.Config.Graph.MaxSteps = %v, want the live 42", rc.Config.Graph.MaxSteps)
			}
			if rc.Config.Graph.NodeTimeout.Duration() != 7*time.Minute {
				t.Errorf("RunContext.Config.Graph.NodeTimeout = %v, want the live 7m",
					rc.Config.Graph.NodeTimeout)
			}
			// The overlay is on a copy: the dispatcher's own configuration
			// is not what gets pinned.
			if live.Fanout.Candidates != 5 || live.Fanout.Enabled {
				t.Errorf("the dispatcher's configuration was mutated: %+v", live.Fanout)
			}
		})
	}
}

func TestRunContextCarriesEverythingANodeIsAllowedToSee(t *testing.T) {
	const runID = "20260101T000000Z-0123456789ab"
	ws := t.TempDir()
	layout, err := state.NewLayout(ws, runID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	var logs bytes.Buffer
	cfg := config.Default()
	agent, runner, linter := &rcAgent{}, &rcRunner{}, &rcLinter{}
	reviewer, isolator, repo := &rcReviewer{}, &rcIsolator{}, &rcVCS{}
	reg := NewRegistry()
	reg.MustRegister(rcIdle{})
	d, err := NewDispatcher(Options{
		Store: state.NewStore(layout), Registry: reg, Config: cfg,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		Agent:  agent, Runner: runner, Linter: linter, Reviewer: reviewer, Isolator: isolator,
		VCS: repo,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	man := state.NewManifest(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), runID, ws, "ship it", cfg)
	// A branch-mode run, because the whole point of Output on the context
	// is that it comes from the manifest rather than from d.cfg, and the
	// zero Output could not tell the two apart.
	man.SetOutput(state.Output{
		Mode: state.OutputBranch, Repo: ws, Branch: "belay/" + runID, Base: "1111111",
		BaseRef: "main", WorkTree: filepath.Join(layout.RunDir(), "worktree"), Prefix: "sub/",
		PullRequest: true,
	})
	// Not the directory belay.yaml says, for the same reason: where a run
	// files its plan comes from what the manifest pinned when it started.
	man.ArchivedPlan.Dir = "notes/plans"
	st := state.NewState("ship it")
	st.Fix.Attempts = 2
	// Spend something, so a Budget that was never filled in cannot pass as
	// one that reports a run with nothing spent yet.
	ledger := budget.New()
	ledger.Record(NodePlan, belay.Usage{InputTokens: 120, OutputTokens: 30, USD: 0.25})

	rc := d.runContext(&man, st, ledger, NodeFix, 5, 2)

	for _, tc := range []struct {
		field     string
		got, want any
	}{
		{"Goal", rc.Goal, "ship it"},
		{"State", rc.State, st},
		{"Config", rc.Config, cfg},
		{"Layout", rc.Layout, layout},
		// Supplied from the layout, never derived by a node: see the loop.
		{"Workspace", rc.Workspace, layout.WorkspaceDir()},
		{"RunID", rc.RunID, runID},
		{"Budget", rc.Budget, ledger.Snapshot(cfg.Budget.MaxUSD)},
		{"NodeName", rc.NodeName, NodeFix},
		{"Step", rc.Step, 5},
		{"Attempt", rc.Attempt, 2},
		{"Agent", rc.Agent, agent},
		{"Runner", rc.Runner, runner},
		{"Linter", rc.Linter, linter},
		{"Reviewer", rc.Reviewer, reviewer},
		{"Isolator", rc.Isolator, isolator},
		// Read from the manifest, not from d.cfg: a run delivers its
		// result the way it was started, whatever belay.yaml says when it
		// is resumed.
		{"Output", rc.Output, man.Output},
		{"ArchiveDir", rc.ArchiveDir, "notes/plans"},
		{"VCS", rc.VCS, repo},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("RunContext.%s = %#v, want %#v", tc.field, tc.got, tc.want)
		}
	}

	// A field added to RunContext without a source here reaches every node
	// as its zero value, and a nil adapter or an empty workspace is read by
	// the node as "not configured". Every field must be set, except the
	// ones below, which are nil on purpose in a run like this one.
	unset := map[string]string{
		// A run that does not fan out has no candidate runs, and no run
		// has any until a caller configures the registry they are driven
		// over. A node that needs one and finds it nil refuses with
		// ErrNoCandidateRunner, which is the same contract every other
		// nil adapter here has; what would be wrong is a node silently
		// doing nothing. The entry stays until the dispatcher builds one.
		"CandidateRuns": "no run fans out yet",
	}
	v := reflect.ValueOf(*rc)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if _, ok := unset[name]; ok {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("RunContext.%s is its zero value: runContext does not set it", name)
		}
	}

	// The logger is scoped to this execution, so every line a node logs can
	// be matched to its journal record.
	if rc.Logger == nil {
		t.Fatal("RunContext.Logger is nil")
	}
	rc.Logger.Info("probe")
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("the node's logger wrote %q, not one JSON line: %v", logs.String(), err)
	}
	for key, want := range map[string]any{"node": NodeFix, "step": 5.0, "attempt": 2.0} {
		if line[key] != want {
			t.Errorf("logged %s = %v, want %v (line: %s)", key, line[key], want, logs.String())
		}
	}
}
