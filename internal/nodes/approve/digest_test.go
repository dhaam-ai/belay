package approve_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
)

// approvedRC returns a gate-ready RunContext whose plan is already approved
// and whose recorded digest matches the plan on disk — the state `belay
// resume` leaves behind when the human changed nothing.
func approvedRC(t *testing.T) *graph.RunContext {
	t.Helper()
	rc := newRC(t, planBody)
	rc.State.Plan.Approved = true
	rc.State.Plan.Digest = approve.Digest([]byte(planBody))
	return rc
}

// editPlan overwrites the plan artifact the way a human with an editor
// would, and returns the digest of the new content.
func editPlan(t *testing.T, rc *graph.RunContext, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rc.Layout.ArtifactsDir(), "plan.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("edit plan: %v", err)
	}
	return approve.Digest([]byte(body))
}

func TestDigestFormat(t *testing.T) {
	got := approve.Digest([]byte(planBody))
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("Digest() = %q, want a \"sha256:\" prefix", got)
	}
	if want := len("sha256:") + 64; len(got) != want {
		t.Errorf("len(Digest()) = %d, want %d", len(got), want)
	}
	if same := approve.Digest([]byte(planBody)); same != got {
		t.Errorf("Digest is not stable: %q then %q", got, same)
	}
	if other := approve.Digest([]byte(planBody + "\n")); other == got {
		t.Error("Digest collided on different content")
	}
}

func TestApprovedRoutesToCode(t *testing.T) {
	res := run(t, approvedRC(t))

	if got, want := res.Next, graph.NodeCode; got != want {
		t.Errorf("Next = %q, want %q", got, want)
	}
	if got, want := res.Status, journal.StatusOK; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
	if res.Patch.Plan != nil {
		t.Errorf("Patch.Plan = %+v, want nil: an unedited plan changes nothing", *res.Patch.Plan)
	}
	if strings.Contains(strings.ToLower(res.Note), "edit") {
		t.Errorf("Note claims an edit that did not happen: %q", res.Note)
	}
}

// The audit-trail case: a human edited plan.md during the pause, so the
// digest State carries no longer describes what the code node will be given.
func TestApprovedWithEditedPlanRecordsNewDigest(t *testing.T) {
	rc := approvedRC(t)
	stale := rc.State.Plan.Digest
	fresh := editPlan(t, rc, planBody+"\n3. Also handle the edge case.\n")

	res := run(t, rc)

	if got, want := res.Next, graph.NodeCode; got != want {
		t.Errorf("Next = %q, want %q: an edit is reported, not rejected", got, want)
	}
	if got, want := res.Status, journal.StatusOK; got != want {
		t.Errorf("Status = %v, want %v", got, want)
	}
	if res.Patch.Plan == nil {
		t.Fatal("Patch.Plan = nil; the edited plan's digest must be recorded")
	}
	if got := res.Patch.Plan.Digest; got != fresh {
		t.Errorf("Patch.Plan.Digest = %q, want %q (the edited content)", got, fresh)
	}
	if res.Patch.Plan.Digest == stale {
		t.Error("Patch carried the stale digest; the audit trail would lie about what was approved")
	}
	if !res.Patch.Plan.Approved || res.Patch.Plan.Path != rc.State.Plan.Path {
		t.Errorf("Patch.Plan clobbered unrelated fields: %+v", *res.Patch.Plan)
	}
	if !strings.Contains(res.Note, "edited") {
		t.Errorf("Note does not say the plan was edited: %q", res.Note)
	}
	if !strings.Contains(res.Note, shortOf(fresh)) {
		t.Errorf("Note %q does not carry the new digest %q", res.Note, shortOf(fresh))
	}
}

// A plan node that never recorded a digest must not make every plan look
// edited; the gate just records the fingerprint it computed.
func TestApprovedWithNoRecordedDigestIsNotAnEdit(t *testing.T) {
	rc := approvedRC(t)
	rc.State.Plan.Digest = ""

	res := run(t, rc)

	if res.Patch.Plan == nil {
		t.Fatal("Patch.Plan = nil; a missing digest must still be filled in")
	}
	if got, want := res.Patch.Plan.Digest, approve.Digest([]byte(planBody)); got != want {
		t.Errorf("Patch.Plan.Digest = %q, want %q", got, want)
	}
	if strings.Contains(strings.ToLower(res.Note), "edited") {
		t.Errorf("Note claims an edit, but nothing was ever fingerprinted: %q", res.Note)
	}
}

// The pause pass records the fingerprint too, so that "edited" on the later
// approved pass means "edited since the human was asked", not "differs from
// whatever the plan node happened to store".
func TestPausePassRecordsDigestWhenStateHasNone(t *testing.T) {
	rc := newRC(t, planBody)
	rc.State.Plan.Digest = ""

	res := run(t, rc)

	if got, want := res.Status, journal.StatusPaused; got != want {
		t.Fatalf("Status = %v, want %v", got, want)
	}
	if res.Patch.Plan == nil {
		t.Fatal("Patch.Plan = nil; the paused gate must record what it showed the human")
	}
	if got, want := res.Patch.Plan.Digest, approve.Digest([]byte(planBody)); got != want {
		t.Errorf("Patch.Plan.Digest = %q, want %q", got, want)
	}
	if res.Patch.Plan.Approved {
		t.Error("the paused gate must not mark the plan approved")
	}
}

// shortOf mirrors the note's abbreviated digest form.
func shortOf(digest string) string {
	const keep = len("sha256:") + 12
	if len(digest) <= keep {
		return digest
	}
	return digest[:keep] + "..."
}
