package state

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestNewCandidateRunLayout pins where a candidate run keeps its own run
// files. They are a sibling of the workspace, not inside it: promote
// destroys a loser's workspace, and an audit trail kept inside one would be
// destroyed with it.
func TestNewCandidateRunLayout(t *testing.T) {
	workspace := t.TempDir()
	parent, err := NewLayout(workspace, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}

	root := filepath.Join(parent.RunDir(), "candidate-runs", "cand-01")
	cases := map[string]string{
		"RunDir":                         child.RunDir(),
		"ManifestPath":                   child.ManifestPath(),
		"StatePath":                      child.StatePath(),
		"JournalPath":                    child.JournalPath(),
		"ArtifactsDir":                   child.ArtifactsDir(),
		"NodesDir":                       child.NodesDir(),
		"SnapshotsDir":                   child.SnapshotsDir(),
		"CandidateRunsDir of the parent": parent.CandidateRunsDir(),
	}
	want := map[string]string{
		"RunDir":                         root,
		"ManifestPath":                   filepath.Join(root, "manifest.json"),
		"StatePath":                      filepath.Join(root, "state.json"),
		"JournalPath":                    filepath.Join(root, "journal.ndjson"),
		"ArtifactsDir":                   filepath.Join(root, "artifacts"),
		"NodesDir":                       filepath.Join(root, "nodes"),
		"SnapshotsDir":                   filepath.Join(root, "snapshots"),
		"CandidateRunsDir of the parent": filepath.Join(parent.RunDir(), "candidate-runs"),
	}
	for k, got := range cases {
		if got != want[k] {
			t.Errorf("%s = %q, want %q", k, got, want[k])
		}
	}

	// The candidate's own run files must not land inside the workspace the
	// isolator makes for it, which is candidates/<id> and which promote may
	// destroy.
	if ws, err := parent.CandidateDir("cand-01"); err != nil {
		t.Fatalf("CandidateDir: %v", err)
	} else if rel, err := filepath.Rel(ws, child.RunDir()); err != nil || rel == "." || !startsWithParent(rel) {
		t.Errorf("candidate run dir %q is inside the candidate's workspace %q", child.RunDir(), ws)
	}
}

// startsWithParent reports whether rel climbs out of the directory it was
// taken relative to, which is what proves one path is not inside another.
func startsWithParent(rel string) bool {
	return rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator)
}

// TestCandidateRunLayoutStartsAtTheParentWorkspace states the one thing the
// new Layout's workspace means before the isolator has run: it is the tree
// the candidate will be isolated from, and the runner replaces it with
// WithWorkTree as soon as there is a workspace of the candidate's own.
func TestCandidateRunLayoutStartsAtTheParentWorkspace(t *testing.T) {
	workspace := t.TempDir()
	parent, err := NewLayout(workspace, "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}
	if got := child.WorkspaceDir(); got != workspace {
		t.Errorf("WorkspaceDir = %q, want the parent's %q", got, workspace)
	}

	own := filepath.Join(parent.CandidatesDir(), "cand-01")
	moved, err := child.WithWorkTree(own)
	if err != nil {
		t.Fatalf("WithWorkTree: %v", err)
	}
	if got := moved.WorkspaceDir(); got != own {
		t.Errorf("WorkspaceDir after WithWorkTree = %q, want %q", got, own)
	}
	if got := moved.RunDir(); got != child.RunDir() {
		t.Errorf("WithWorkTree moved RunDir to %q, want it left at %q", got, child.RunDir())
	}
	if got := moved.JournalPath(); got != child.JournalPath() {
		t.Errorf("WithWorkTree moved JournalPath to %q, want it left at %q", got, child.JournalPath())
	}
}

// TestNewCandidateRunLayoutRefusesAnUnsafeID: a candidate id reaches this
// constructor from the fanout node, and an id that could escape the parent
// run directory would put a whole run's files somewhere nobody is looking.
// It is refused rather than cleaned, for the reason validateSegment gives.
func TestNewCandidateRunLayoutRefusesAnUnsafeID(t *testing.T) {
	parent, err := NewLayout(t.TempDir(), "run-1")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}

	for _, id := range []string{"", "..", "../escape", "a/b", "a\\b", "/etc/passwd", "cand\x0001"} {
		t.Run(id, func(t *testing.T) {
			if _, err := NewCandidateRunLayout(parent, id); !errors.Is(err, ErrInvalidPathSegment) {
				t.Errorf("NewCandidateRunLayout(parent, %q) error = %v, want ErrInvalidPathSegment", id, err)
			}
		})
	}
}

// TestNewCandidateRunLayoutRefusesAZeroParent: a zero Layout has no run
// directory, so every path it builds is relative to whatever directory belay
// happens to be running in. A candidate run built on one would scatter a
// whole run's files there.
func TestNewCandidateRunLayoutRefusesAZeroParent(t *testing.T) {
	if _, err := NewCandidateRunLayout(Layout{}, "cand-01"); !errors.Is(err, ErrInvalidPathSegment) {
		t.Errorf("NewCandidateRunLayout(Layout{}, ...) error = %v, want ErrInvalidPathSegment", err)
	}
}

// TestCandidateRunIdentityLivesInTheManifestNotTheLayout is a guard, not a
// RED: Layout.RunID() is filepath.Base(root) today, so a candidate run's
// Layout knows only the bare id. The run's full identity — parent and
// candidate — lives in its manifest, which is where the dispatcher reads a
// run's ID from. The test exists so that a later change "fixing"
// Layout.RunID to return something qualified is caught here rather than in
// whatever it renames on disk.
func TestCandidateRunIdentityLivesInTheManifestNotTheLayout(t *testing.T) {
	parent, err := NewLayout(t.TempDir(), "20260101T000000Z-c0ffee000000")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	child, err := NewCandidateRunLayout(parent, "cand-01")
	if err != nil {
		t.Fatalf("NewCandidateRunLayout: %v", err)
	}

	if got, want := child.RunID(), "cand-01"; got != want {
		t.Errorf("child Layout.RunID() = %q, want the bare candidate id %q", got, want)
	}
	if got, want := parent.RunID(), "20260101T000000Z-c0ffee000000"; got != want {
		t.Errorf("parent Layout.RunID() = %q, want %q", got, want)
	}

	// The identity a reader of the candidate's manifest sees is the
	// qualified one, and the two must not be confused: one names a
	// directory, the other names a run.
	qualified := parent.RunID() + "-" + child.RunID()
	if qualified == child.RunID() {
		t.Error("the candidate run's manifest id is indistinguishable from its directory name")
	}
}

// TestFanoutBlocksJSONKeySets is a guard, not a RED: it locks the on-disk
// field set of the two blocks a fanout run's manifests carry, so a rename or
// an addition is a reviewed change rather than a silent one.
func TestFanoutBlocksJSONKeySets(t *testing.T) {
	assertExactJSONKeys(t, FanoutPin{}, []string{"candidates", "select"})
	assertExactJSONKeys(t, Envelope{}, []string{"limit_usd", "limit_tokens"})
	assertExactJSONKeys(t, CandidateRun{}, []string{
		"parent_run_id", "id", "index", "workspace", "provision", "provision_note", "envelope",
	})
}

// TestManifest_SetFanoutRecordsTheCapabilityAFanoutRunNeeds is ADR-0011
// applied to the pin that decides a run's shape. Manifest.Fanout is a
// control field: a belay that did not know it would decode the manifest,
// drop the pin, resume the run as an ordinary single run — spending the
// whole ceiling again on one attempt — and write the manifest back without
// it. Naming the capability turns that into a refusal that says to upgrade.
func TestManifest_SetFanoutRecordsTheCapabilityAFanoutRunNeeds(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	pin := FanoutPin{Candidates: 3, Select: "fewest_issues"}

	m.SetFanout(pin)

	if m.Fanout == nil {
		t.Fatal("SetFanout left Manifest.Fanout nil")
	}
	if diff := cmp.Diff(pin, *m.Fanout); diff != "" {
		t.Errorf("Fanout (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{CapabilityFanoutCandidateRuns}, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}

	// The manifest is rewritten after every node, so pinning the same
	// shape twice must not accumulate.
	m.SetFanout(pin)
	if diff := cmp.Diff([]string{CapabilityFanoutCandidateRuns}, m.Requires); diff != "" {
		t.Errorf("Requires after a second SetFanout (-want +got):\n%s", diff)
	}
}

// TestManifest_SetCandidateRecordsTheCapabilityACandidateRunNeeds is the
// same decision on the other document, where losing the block would be
// worse: the envelope lives there, so a belay that dropped it would resume a
// candidate with no ceiling of its own and let it spend the parent's whole
// budget.
func TestManifest_SetCandidateRecordsTheCapabilityACandidateRunNeeds(t *testing.T) {
	m := NewManifest(time.Now(), "run-1-cand-01", "/work", "goal", config.Default())
	cand := CandidateRun{
		ParentRunID: "run-1",
		ID:          "cand-01",
		Index:       1,
		Workspace:   belay.Workspace{ID: "cand-01", Dir: "/work/.belay/runs/run-1/candidates/cand-01", Ephemeral: true},
		Provision:   ProvisionOK,
		Envelope:    Envelope{LimitUSD: 1.5, LimitTokens: 200000},
	}

	m.SetCandidate(cand)

	if m.Candidate == nil {
		t.Fatal("SetCandidate left Manifest.Candidate nil")
	}
	if diff := cmp.Diff(cand, *m.Candidate); diff != "" {
		t.Errorf("Candidate (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{CapabilityFanoutCandidateRuns}, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}

	m.SetCandidate(cand)
	if diff := cmp.Diff([]string{CapabilityFanoutCandidateRuns}, m.Requires); diff != "" {
		t.Errorf("Requires after a second SetCandidate (-want +got):\n%s", diff)
	}
}

// TestManifest_SetFanoutLeavesOtherCapabilitiesAlone: the requires list is
// shared, and a fanout run on a branch depends on both fields.
func TestManifest_SetFanoutLeavesOtherCapabilitiesAlone(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	m.SetOutput(Output{Mode: OutputBranch, Branch: "belay/run-1"})

	m.SetFanout(FanoutPin{Candidates: 3, Select: "fewest_issues"})

	want := []string{CapabilityFanoutCandidateRuns, CapabilityOutputBranch}
	if diff := cmp.Diff(want, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
}

// TestManifestWithoutFanoutCarriesNoBlocks is what makes this slice safe to
// merge on its own: a run that does not fan out writes exactly the
// manifest.json it wrote before these fields existed, and requires nothing.
func TestManifestWithoutFanoutCarriesNoBlocks(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"fanout", "candidate"} {
		if _, ok := keys[absent]; ok {
			t.Errorf("an ordinary run's manifest carries a %q block: %s", absent, data)
		}
	}
	if diff := cmp.Diff([]string{}, m.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
}

// TestFanoutManifestsRoundTrip proves the two documents a fanout run writes
// can be read back by the belay that wrote them: the capability they declare
// has to be one this binary knows, or the first resume would refuse the run
// it had just created.
func TestFanoutManifestsRoundTrip(t *testing.T) {
	dir := t.TempDir()

	parent := NewManifest(time.Now().UTC().Truncate(time.Second), "run-1", "/work", "goal", config.Default())
	parent.SetFanout(FanoutPin{Candidates: 3, Select: "fastest"})
	parentPath := filepath.Join(dir, "parent.json")
	if err := SaveManifest(parentPath, parent); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	gotParent, err := LoadManifest(parentPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if diff := cmp.Diff(parent, gotParent); diff != "" {
		t.Errorf("parent manifest round trip (-saved +loaded):\n%s", diff)
	}
	if gotParent.Fanout == nil {
		t.Error("the parent manifest came back without its fanout pin")
	}
	if !slices.Contains(gotParent.Requires, CapabilityFanoutCandidateRuns) {
		t.Errorf("Requires = %v, want the fanout capability declared", gotParent.Requires)
	}

	child := NewManifest(time.Now().UTC().Truncate(time.Second), "run-1-cand-02", "/work", "goal", config.Default())
	child.SetCandidate(CandidateRun{
		ParentRunID:   "run-1",
		ID:            "cand-02",
		Index:         2,
		Provision:     ProvisionRefused,
		ProvisionNote: "the source tree has uncommitted changes",
		Envelope:      Envelope{LimitUSD: 0.333333, LimitTokens: 33333},
	})
	childPath := filepath.Join(dir, "child.json")
	if err := SaveManifest(childPath, child); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	gotChild, err := LoadManifest(childPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if diff := cmp.Diff(child, gotChild); diff != "" {
		t.Errorf("candidate manifest round trip (-saved +loaded):\n%s", diff)
	}
	if gotChild.Candidate == nil {
		t.Error("the candidate manifest came back without its candidate block")
	}
	if !slices.Contains(gotChild.Requires, CapabilityFanoutCandidateRuns) {
		t.Errorf("Requires = %v, want the fanout capability declared", gotChild.Requires)
	}
}

// TestProvisionPendingIsTheZeroValue states what a candidate block with no
// provision recorded has to mean. Pending is "claimed, no workspace yet",
// which a resume retries; refused is durable and is reproduced forever. A
// zero value that read as refused would lose a candidate to a torn write,
// and one that read as ok would build a dispatcher over a workspace that
// does not exist.
func TestProvisionPendingIsTheZeroValue(t *testing.T) {
	var c CandidateRun
	if c.Provision != ProvisionPending {
		t.Errorf("the zero CandidateRun's Provision = %q, want pending", c.Provision)
	}
	if err := json.Unmarshal([]byte(`{"id":"cand-01","index":1}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Provision != ProvisionPending {
		t.Errorf("a candidate block with no provision key decoded as %q, want pending", c.Provision)
	}
}

// TestPinnedConfigOverlaysTheFanoutPinAndNotTheLimits is the rule that
// decides what a node reads. The graph's shape is pinned when the run is
// created, so a node cannot be written that reads the live fanout settings
// by accident; the dispatcher's limits are deliberately left live, because
// the settled rule is that a person may raise one and resume a run that met
// it.
func TestPinnedConfigOverlaysTheFanoutPinAndNotTheLimits(t *testing.T) {
	live := config.Default()
	live.Fanout = config.Fanout{
		Enabled:    false,
		Candidates: 5,
		Isolator:   config.IsolatorDircopy,
		Select:     config.SelectFastest,
	}
	live.Budget = config.Budget{MaxUSD: 50, MaxTokens: 900000, OnExceed: config.OnExceedWarn}
	live.Graph.MaxSteps = 99
	live.Graph.NodeTimeout = config.NewDuration(42 * time.Minute)

	tests := []struct {
		name       string
		pin        *FanoutPin
		wantFanout config.Fanout
	}{
		{
			name:       "no pin leaves the configuration alone",
			pin:        nil,
			wantFanout: live.Fanout,
		},
		{
			name: "a pin decides enabled, candidates and select",
			pin:  &FanoutPin{Candidates: 3, Select: string(config.SelectFewestIssues)},
			wantFanout: config.Fanout{
				Enabled:    true,
				Candidates: 3,
				// The isolator is not part of the graph's shape: it is how a
				// workspace is made, and the CLI has already resolved it.
				Isolator: config.IsolatorDircopy,
				Select:   config.SelectFewestIssues,
			},
		},
		{
			name: "fanout.enabled turned off under a pinned run does not unpin it",
			pin:  &FanoutPin{Candidates: 2, Select: string(config.SelectFirstPass)},
			wantFanout: config.Fanout{
				Enabled:    true,
				Candidates: 2,
				Isolator:   config.IsolatorDircopy,
				Select:     config.SelectFirstPass,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
			if tt.pin != nil {
				m.SetFanout(*tt.pin)
			}

			got := m.PinnedConfig(live)

			if diff := cmp.Diff(tt.wantFanout, got.Fanout); diff != "" {
				t.Errorf("Fanout (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(live.Budget, got.Budget); diff != "" {
				t.Errorf("budget.* must stay live so a raised ceiling reaches a resumed run (-live +got):\n%s", diff)
			}
			if got.Graph.MaxSteps != live.Graph.MaxSteps {
				t.Errorf("graph.max_steps = %d, want the live %d", got.Graph.MaxSteps, live.Graph.MaxSteps)
			}
			if got.Graph.NodeTimeout.Duration() != live.Graph.NodeTimeout.Duration() {
				t.Errorf("graph.node_timeout = %v, want the live %v",
					got.Graph.NodeTimeout.Duration(), live.Graph.NodeTimeout.Duration())
			}
		})
	}
}

// TestPinnedConfigDoesNotMutateTheConfigItIsGiven: the dispatcher holds one
// Config for the process and calls this once per node. Overlaying in place
// would pin the live configuration itself, and every later reader — a second
// run in the same process, `belay runs` — would see a fanout nobody asked
// for.
func TestPinnedConfigDoesNotMutateTheConfigItIsGiven(t *testing.T) {
	live := config.Default()
	before := live

	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	m.SetFanout(FanoutPin{Candidates: 4, Select: string(config.SelectFewestIssues)})
	_ = m.PinnedConfig(live)

	if diff := cmp.Diff(before.Fanout, live.Fanout); diff != "" {
		t.Errorf("PinnedConfig changed the configuration it was given (-before +after):\n%s", diff)
	}
}

// TestPinnedConfigCarriesAStrategyThisBelayDoesNotKnow: the pin is a string
// so that a manifest written by a newer belay names its strategy rather than
// failing to decode. Overlaying it unchanged is what lets join report an
// unknown strategy itself, in one place, instead of this function guessing a
// default and silently picking a different winner.
func TestPinnedConfigCarriesAStrategyThisBelayDoesNotKnow(t *testing.T) {
	m := NewManifest(time.Now(), "run-1", "/work", "goal", config.Default())
	m.SetFanout(FanoutPin{Candidates: 3, Select: "cheapest"})

	got := m.PinnedConfig(config.Default())

	if got.Fanout.Select != config.SelectStrategy("cheapest") {
		t.Errorf("Select = %q, want it carried through as written", got.Fanout.Select)
	}
}

// TestSeedCandidateStateCarriesOnlyGoalAndPlan pins what a candidate run
// starts from. A candidate re-does the work from the plan, so it gets the
// goal and the plan and nothing else: a naive copy would hand every
// candidate the parent's agent session — making N candidates continue one
// conversation, and one another's — and would seed each with the parent's
// test and review results, which belong to code no candidate wrote.
func TestSeedCandidateStateCarriesOnlyGoalAndPlan(t *testing.T) {
	parent := NewState("ship the widget")
	parent.Plan = Plan{Path: "plan.md", Approved: true, Digest: "sha256:aaa"}
	parent.Code = Code{SessionID: "sess-1", LastDiff: "artifacts/diff-0001.patch", ChangedFiles: []string{"a.go"}}
	parent.Test = NewTest(belay.TestReport{Total: 5, Passed: 5}, "artifacts/test.json")
	parent.Review = NewReview(belay.QualityReport{Source: "golangci-lint", Gate: belay.GatePass}, "")
	parent.Fix = Fix{Attempts: 2}
	parent.Candidates = []Candidate{{ID: "cand-01"}}
	parent.Winner = "cand-01"
	parent.Promotion = Promotion{Branch: "belay/run-1", Commit: "abc123"}
	parent.History = []HistoryEntry{{Seq: 1, Node: "plan", Attempt: 1, Status: "ok"}}

	seed := SeedCandidateState(parent)

	want := NewState("ship the widget")
	want.Plan = Plan{Path: "plan.md", Approved: true, Digest: "sha256:aaa"}
	if diff := cmp.Diff(want, seed); diff != "" {
		t.Errorf("SeedCandidateState (-want +got):\n%s", diff)
	}
	if seed.Code.SessionID != "" {
		t.Errorf("Code.SessionID = %q, want the parent's agent session left behind", seed.Code.SessionID)
	}
}

// TestSeedCandidateStateDropsAnUnservedPlanRequest: a candidate run has no
// plan node and no approve gate, so a request for a new plan carried into
// one could never be served — and the run would declare a capability for a
// field nothing in it reads.
func TestSeedCandidateStateDropsAnUnservedPlanRequest(t *testing.T) {
	parent := NewState("ship the widget")
	parent.Plan = Plan{Path: "plan.md", Approved: true, Digest: "sha256:aaa"}
	parent.SetRegeneration(Regeneration{Request: 2, Guidance: "fewer steps", Served: 1})

	seed := SeedCandidateState(parent)

	if seed.Plan.Regenerate.Pending() {
		t.Errorf("the seed carries a pending request for a new plan: %+v", seed.Plan.Regenerate)
	}
	if diff := cmp.Diff([]string{}, seed.Requires); diff != "" {
		t.Errorf("Requires (-want +got):\n%s", diff)
	}
}

// TestSeedCandidateStateIsAValidNewDocument: the seed is written straight to
// a candidate run's state.json, so it has to be what NewState would produce
// — current schema, and empty lists rather than nulls.
func TestSeedCandidateStateIsAValidNewDocument(t *testing.T) {
	seed := SeedCandidateState(NewState("ship the widget"))

	if seed.SchemaVersion != StateSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", seed.SchemaVersion, StateSchemaVersion)
	}
	for name, list := range map[string]int{
		"Requires":   len(seed.Requires),
		"Candidates": len(seed.Candidates),
		"History":    len(seed.History),
	} {
		if list != 0 {
			t.Errorf("%s = %d entries, want none", name, list)
		}
	}
	if seed.Requires == nil || seed.Candidates == nil || seed.History == nil {
		t.Errorf("a nil list would write null where every other run writes []: %+v", seed)
	}
}
