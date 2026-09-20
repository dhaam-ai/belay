package approve_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
)

// TestApprovalDisabledRoutesToCode documents and pins the decision for a
// case that should be unreachable.
//
// With graph.approval off, the plan node routes straight to code and this
// node never runs. If it runs anyway the wiring is broken, and the gate
// still has to pick a behaviour. Pausing would strand a run the user asked
// to be unattended in a state only a human at a terminal can clear — the
// exact outcome the flag exists to prevent. So the gate proceeds, warns, and
// records the anomaly in the note, where the journal keeps the evidence.
func TestApprovalDisabledRoutesToCode(t *testing.T) {
	rc := newRC(t, planBody)
	rc.Config.Graph.Approval = false
	if rc.State.Plan.Approved {
		t.Fatal("test setup: the plan must be unapproved for this case to mean anything")
	}

	res := run(t, rc)

	if got, want := res.Next, graph.NodeCode; got != want {
		t.Errorf("Next = %q, want %q: an unattended run must not be parked", got, want)
	}
	if got, want := res.Status, journal.StatusOK; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
	for _, want := range []string{"graph.approval=false", "wiring bug"} {
		if !strings.Contains(res.Note, want) {
			t.Errorf("Note does not mention %q, so the anomaly would be invisible: %q", want, res.Note)
		}
	}
}

// An approved plan is approved regardless of the flag, and must not be
// reported as a wiring bug.
func TestApprovalDisabledWithApprovedPlanUsesTheOrdinaryNote(t *testing.T) {
	rc := approvedRC(t)
	rc.Config.Graph.Approval = false

	res := run(t, rc)

	if got, want := res.Next, graph.NodeCode; got != want {
		t.Errorf("Next = %q, want %q", got, want)
	}
	if strings.Contains(res.Note, "wiring bug") {
		t.Errorf("an approved plan is not a wiring bug: %q", res.Note)
	}
}

// TestIdempotent covers the dispatcher's re-run contract: a node whose
// node_started has no matching node_finished is executed again on resume, so
// every path here must produce the same verdict the second time.
func TestIdempotent(t *testing.T) {
	tests := []struct {
		name       string
		rc         func(t *testing.T) *graph.RunContext
		wantStatus journal.Status
		wantNext   string
	}{
		{
			name:       "unapproved gate pauses again with the same note",
			rc:         func(t *testing.T) *graph.RunContext { t.Helper(); return newRC(t, planBody) },
			wantStatus: journal.StatusPaused,
			wantNext:   "",
		},
		{
			name:       "approved gate routes to code again",
			rc:         approvedRC,
			wantStatus: journal.StatusOK,
			wantNext:   graph.NodeCode,
		},
		{
			name: "approval-disabled gate routes to code again",
			rc: func(t *testing.T) *graph.RunContext {
				t.Helper()
				rc := newRC(t, planBody)
				rc.Config.Graph.Approval = false
				return rc
			},
			wantStatus: journal.StatusOK,
			wantNext:   graph.NodeCode,
		},
		{
			name: "edited-plan gate reports the same new digest again",
			rc: func(t *testing.T) *graph.RunContext {
				t.Helper()
				rc := approvedRC(t)
				editPlan(t, rc, planBody+"\n3. And one more thing.\n")
				return rc
			},
			wantStatus: journal.StatusOK,
			wantNext:   graph.NodeCode,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := tt.rc(t)

			// Nothing between the two calls updates State: the dispatcher
			// applies patches, and a re-run after a crash sees the same
			// snapshot the first attempt saw.
			first := run(t, rc)
			rc.Attempt = 2
			second := run(t, rc)

			if diff := cmp.Diff(first, second); diff != "" {
				t.Errorf("re-running the gate changed its verdict (-first +second):\n%s", diff)
			}
			if first.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", first.Status, tt.wantStatus)
			}
			if first.Next != tt.wantNext {
				t.Errorf("Next = %q, want %q", first.Next, tt.wantNext)
			}
		})
	}
}

// TestWritesNothing enforces ADR-0002 directly: a node returns a Patch and
// never persists anything itself. This walks the entire run directory before
// and after Run and requires it to be byte-identical, which catches a write
// to state.json, manifest.json or journal.ndjson — and any other file — no
// matter how it was made.
func TestWritesNothing(t *testing.T) {
	for _, tt := range []struct {
		name string
		rc   func(t *testing.T) *graph.RunContext
	}{
		{"paused", func(t *testing.T) *graph.RunContext { t.Helper(); return newRC(t, planBody) }},
		{"approved", approvedRC},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rc := tt.rc(t)

			// Give the node every file it could plausibly clobber.
			for _, name := range []string{"state.json", "manifest.json", "journal.ndjson"} {
				p := filepath.Join(rc.Layout.RunDir(), name)
				if err := os.WriteFile(p, []byte("{\"sentinel\":true}\n"), 0o600); err != nil {
					t.Fatalf("seed %s: %v", name, err)
				}
			}

			before := snapshot(t, rc.Layout.RunDir())
			run(t, rc)
			after := snapshot(t, rc.Layout.RunDir())

			if diff := cmp.Diff(before, after); diff != "" {
				t.Errorf("the gate wrote to the run directory, violating ADR-0002 (-before +after):\n%s", diff)
			}
		})
	}
}

// snapshot fingerprints every file under root by relative path and content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path) //nolint:gosec // test-owned temp dir
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("snapshot of %s is empty; the comparison would pass vacuously", root)
	}
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("run dir holds %d files: %v", len(names), names)
	return out
}

// TestUnapprovablePlanPausesInsteadOfEndingTheRun covers the file a person
// broke while the run was paused: they emptied plan.md, or saved it in an
// encoding that is not UTF-8. Until now the gate returned an error for
// that, the dispatcher ended the run, and `belay resume` refused the run
// afterwards -- so an editing slip cost the whole run and everything it had
// spent, although the only thing wrong was a file the person still had open.
// The plan is theirs to fix, so the gate pauses and says what is wrong.
func TestUnapprovablePlanPausesInsteadOfEndingTheRun(t *testing.T) {
	tests := []struct {
		name       string
		plan       string
		approved   bool
		wantReason string
	}{
		{"emptied", "   \n\t\n", false, "is empty"},
		{"emptied after it was approved", "", true, "is empty"},
		{"saved as something that is not UTF-8", "# Plan\n\n1. \xff\xfe do the thing\n", false, "is not valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := newRC(t, planBody)
			rc.State.Plan.Approved = tt.approved
			rc.State.Plan.Digest = approve.Digest([]byte(planBody))
			editPlan(t, rc, tt.plan)

			res, err := approve.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("Run: %v; the gate must pause on a plan a person can fix, not end the run", err)
			}
			if verr := res.Validate(); verr != nil {
				t.Fatalf("Run returned a Result the dispatcher cannot act on: %v", verr)
			}
			if got, want := res.Status, journal.StatusPaused; got != want {
				t.Errorf("Status = %v, want %v", got, want)
			}
			if res.Next != "" {
				t.Errorf("Next = %q, want empty: an unapprovable plan routes nowhere", res.Next)
			}
			plan := filepath.Join(rc.Layout.ArtifactsDir(), "plan.md")
			for _, want := range []string{plan, tt.wantReason, "belay resume " + testRunID} {
				if !strings.Contains(res.Note, want) {
					t.Errorf("Note does not carry %q:\n%s", want, res.Note)
				}
			}
			if strings.Contains(res.Note, "\n") {
				t.Errorf("Note must stay one line so it reads in a timeline:\n%q", res.Note)
			}
			// The digest on the blackboard describes the plan the person
			// was shown. Recording the broken file's fingerprint would make
			// the run's record claim they approved these bytes.
			if res.Patch.Plan != nil {
				t.Errorf("Patch.Plan = %+v, want none: nothing approvable was read", *res.Patch.Plan)
			}

			// A dispatcher re-runs a node whose start has no finish.
			rc.Attempt = 2
			again, err := approve.New().Run(context.Background(), rc)
			if err != nil {
				t.Fatalf("re-run: %v", err)
			}
			if diff := cmp.Diff(res, again); diff != "" {
				t.Errorf("re-running the gate changed its verdict (-first +second):\n%s", diff)
			}
		})
	}
}
