package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/planview"
	"github.com/dhaam-ai/belay/internal/state"
)

// scriptedReviewer stands in for the viewer: it answers each time it is
// opened with the next decision, after doing whatever the script says with
// the options it was given.
type scriptedReviewer struct {
	answers []planview.Decision
	err     error
	opens   []planview.Options
	// edit, when set, is saved through the options the viewer was given
	// before the answer is returned, as a person editing and approving.
	edit []byte
	// onOpen runs each time the viewer is opened.
	onOpen func()
}

func (s *scriptedReviewer) review(_ context.Context, _, _ *os.File, opts planview.Options) (planview.Decision, error) {
	s.opens = append(s.opens, opts)
	if s.onOpen != nil {
		s.onOpen()
	}
	if s.err != nil {
		return planview.Decision{}, s.err
	}
	if s.edit != nil {
		if err := opts.Save(s.edit, false); err != nil {
			return planview.Decision{}, err
		}
		s.edit = nil
	}
	if len(s.answers) == 0 {
		return planview.Decision{}, nil
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

// reviewUI is a ui a viewer may open on, writing to out.
func reviewUI(out *bytes.Buffer, r *scriptedReviewer) *ui {
	return &ui{
		out:    out,
		err:    out,
		viewer: viewerTerminal{ok: true, in: os.Stdin, out: os.Stdout},
		review: r.review,
	}
}

// pausedRun is a run of the real plan and approve nodes plus a node
// standing in for everything after them, ready to be driven.
func pausedRun(t *testing.T, u *ui, plans ...string) (*runPlan, *state.Store, *bool) {
	t.Helper()
	p, err := resolve(resolveRequest{workspace: goWorkspace(t), logger: newLogger(u)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	p.goal = "add tests"
	p.agent = regenAgent(plans...)
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(plan.New(), approve.New(), endsTheRun{reached: &reached})
	p.registry = registry
	return p, seedRunDir(t, p, "add tests"), &reached
}

// cmdWithContext is the cobra command a driven run is given.
func cmdWithContext() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

const firstPlan = "# Plan\n\n## Steps\n\n1. Rewrite the parser.\n2. Rewrite the tests.\n"

// TestTheViewerApprovesTheEditedPlanThroughTheResumePath: what a person
// edits in the viewer is what the run goes on to build, and it gets there
// the way `belay resume` gets there — the plan file, the gate's own
// digest — rather than by any path of the viewer's own.
func TestTheViewerApprovesTheEditedPlanThroughTheResumePath(t *testing.T) {
	edited := []byte("# Plan\n\n## Steps\n\n1. Add one test.\n")
	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionApprove}}, edit: edited}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	p, store, reached := pausedRun(t, u, firstPlan)

	if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if !*reached {
		t.Error("approving in the viewer did not continue the run")
	}
	if len(reviewer.opens) != 1 {
		t.Fatalf("the viewer was opened %d time(s), want once", len(reviewer.opens))
	}
	opened := reviewer.opens[0]
	if string(opened.Plan) != firstPlan {
		t.Errorf("the viewer was given %q, want the plan the gate stopped at", opened.Plan)
	}
	if opened.RunID != store.Layout().RunID() || opened.Goal != "add tests" {
		t.Errorf("the viewer was told run %q, goal %q", opened.RunID, opened.Goal)
	}
	if opened.Mode != planview.Review || opened.Blocker != "" {
		t.Errorf("the viewer was opened in mode %v with blocker %q, want a reviewable plan", opened.Mode, opened.Blocker)
	}

	onDisk := readFile(t, filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName))
	if onDisk != string(edited) {
		t.Errorf("the plan file holds %q, want what the viewer saved", onDisk)
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.Plan.Approved {
		t.Error("the run went on without the approval being recorded")
	}
	if st.Plan.Digest != approve.Digest(edited) {
		t.Errorf("the recorded digest is %q, want the gate's fingerprint of the edited plan", st.Plan.Digest)
	}
	if got := out.String(); !strings.Contains(got, "the plan is approved") {
		t.Errorf("the run never says the plan was approved:\n%s", got)
	}
}

// TestAskingForANewPlanInTheViewerIsTheSameJournalledEdge: the viewer's
// request goes through the run, not around it — plan, approve, approve,
// plan, approve in the journal — and the viewer opens again on the new
// plan, so the person reads what they asked for.
func TestAskingForANewPlanInTheViewerIsTheSameJournalledEdge(t *testing.T) {
	const secondPlan = "# Plan\n\n## Steps\n\n1. Add one test.\n"
	reviewer := &scriptedReviewer{answers: []planview.Decision{
		{Action: planview.ActionRegenerate, Guidance: "fewer steps"},
		{Action: planview.ActionLeave},
	}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	p, store, reached := pausedRun(t, u, firstPlan, secondPlan)

	if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if *reached {
		t.Error("the run went past the gate; nobody approved the new plan")
	}
	if len(reviewer.opens) != 2 {
		t.Fatalf("the viewer was opened %d time(s), want twice: once per plan", len(reviewer.opens))
	}
	if got := string(reviewer.opens[1].Plan); got != secondPlan {
		t.Errorf("the viewer was opened again on %q, want the new plan", got)
	}
	want := []string{graph.NodePlan, graph.NodeApprove, graph.NodeApprove, graph.NodePlan, graph.NodeApprove}
	if got := journalNodes(t, store); !slices.Equal(got, want) {
		t.Errorf("the journal records %v, want %v", got, want)
	}
	kept := readFile(t, filepath.Join(store.Layout().ArtifactsDir(), plan.ReplacedArtifact(1)))
	if kept != firstPlan {
		t.Errorf("the plan the request replaced was not kept: %q", kept)
	}
	got := out.String()
	if !strings.Contains(got, "belay resume "+store.Layout().RunID()) {
		t.Errorf("the run does not end with the command that continues it:\n%s", got)
	}
}

// TestLeavingTheViewerLeavesTheRunWaiting: closing the viewer decides
// nothing. The run is where the gate left it, and belay says how to come
// back — including that the viewer itself is one way.
func TestLeavingTheViewerLeavesTheRunWaiting(t *testing.T) {
	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionLeave}}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	p, store, reached := pausedRun(t, u, firstPlan)

	// The state as the gate left it, read when the viewer opens: the viewer
	// may write the plan file, and nothing else about the run.
	var before string
	reviewer.onOpen = func() { before = readFile(t, store.Layout().StatePath()) }
	if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
		t.Fatalf("driveRun: %v", err)
	}
	if *reached {
		t.Error("leaving the viewer continued the run")
	}
	if after := readFile(t, store.Layout().StatePath()); after != before {
		t.Errorf("leaving the viewer changed the run's state:\n%s", after)
	}
	got := out.String()
	for _, want := range []string{"waiting for you", "belay resume " + store.Layout().RunID()} {
		if !strings.Contains(got, want) {
			t.Errorf("the report does not say %q:\n%s", want, got)
		}
	}
}

// TestAViewerThatFailsLeavesTheRunExactlyAsThePauseLeftIt: the viewer is
// not part of the run. If it cannot open, or breaks, belay says so on
// stderr and reports the pause as it always has, with the exit status a
// pause has always had.
func TestAViewerThatFailsLeavesTheRunExactlyAsThePauseLeftIt(t *testing.T) {
	reviewer := &scriptedReviewer{err: errors.New("the terminal went away")}
	var out, errOut bytes.Buffer
	u := reviewUI(&out, reviewer)
	u.err = &errOut
	p, store, _ := pausedRun(t, u, firstPlan)

	if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
		t.Fatalf("driveRun = %v, want nil: the run paused cleanly", err)
	}
	if !strings.Contains(errOut.String(), "the terminal went away") {
		t.Errorf("the failure is not reported on stderr:\n%s", errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "belay resume "+store.Layout().RunID()) {
		t.Errorf("the report is not the ordinary pause report:\n%s", got)
	}
	assertPlainText(t, "a run whose viewer failed", out.String())
}

// TestTheViewerNeverOpensWhereItMayNot is the headless promise: with no
// terminal to open on, belay never reaches for the viewer at all. The
// reviewer here fails the test if it is called.
func TestTheViewerNeverOpensWhereItMayNot(t *testing.T) {
	for _, term := range []viewerTerminal{
		{},
		{reason: "--no-input was given"},
		{reason: "CI is set, so belay takes it that nobody is at the terminal"},
	} {
		var out bytes.Buffer
		u := &ui{out: &out, err: &out, viewer: term}
		u.review = func(context.Context, *os.File, *os.File, planview.Options) (planview.Decision, error) {
			t.Fatalf("the viewer was opened although it may not be: %q", term.reason)
			return planview.Decision{}, nil
		}
		p, store, reached := pausedRun(t, u, firstPlan)
		if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
			t.Fatalf("driveRun: %v", err)
		}
		if *reached {
			t.Error("the run went past the gate with nobody to approve it")
		}
		got := out.String()
		assertPlainText(t, "a run with no terminal", got)
		for _, want := range []string{
			"belay paused after 2 steps, and is waiting for you.",
			filepath.Join(store.Layout().ArtifactsDir(), "plan.md"),
			"read the plan, edit it if you want to, then run:",
			"belay resume " + store.Layout().RunID(),
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the pause report is not what it has always been; %q is missing:\n%s", want, got)
			}
		}
	}
}

// TestTheViewerSavesThroughTheGatesOwnPathAndRule: the viewer's save is
// the hand-edit path in every respect — the same file, the same rule about
// what may be in it — and it refuses to overwrite an edit made elsewhere
// while it was open.
func TestTheViewerSavesThroughTheGatesOwnPathAndRule(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	file, err := openPlanFile(store)
	if err != nil {
		t.Fatalf("openPlanFile: %v", err)
	}
	if want := filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName); file.path != want {
		t.Errorf("the viewer would write to %q, want the file the gate reads, %q", file.path, want)
	}

	if err := file.save([]byte("   \n"), false); !errors.Is(err, approve.ErrPlanEmpty) {
		t.Errorf("saving an empty plan = %v, want the gate's own refusal", err)
	}

	edited := []byte("# Plan\n\n## Steps\n\n1. Write the test.\n2. Make it pass, quickly.\n")
	if err := file.save(edited, false); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := readFile(t, file.path); got != string(edited) {
		t.Errorf("the plan file holds %q, want the edit", got)
	}

	// Somebody saves the file from their own editor while the viewer is open.
	elsewhere := []byte("# Plan\n\n## Steps\n\n1. What the other editor wrote.\n")
	if err := os.WriteFile(file.path, elsewhere, 0o600); err != nil {
		t.Fatalf("write the plan from elsewhere: %v", err)
	}
	if err := file.save(edited, false); !errors.Is(err, planview.ErrChangedOnDisk) {
		t.Errorf("save after an edit elsewhere = %v, want ErrChangedOnDisk", err)
	}
	if got := readFile(t, file.path); got != string(elsewhere) {
		t.Errorf("the refused save wrote anyway: %q", got)
	}
	again, err := file.reload()
	if err != nil || !bytes.Equal(again, elsewhere) {
		t.Fatalf("reload = (%q, %v), want the file as it is now", again, err)
	}
	if err := file.save(edited, false); err != nil {
		t.Errorf("save after reading the file again = %v, want it to go through", err)
	}
	if err := os.WriteFile(file.path, elsewhere, 0o600); err != nil {
		t.Fatalf("write the plan from elsewhere: %v", err)
	}
	if err := file.save(edited, true); err != nil {
		t.Errorf("save with overwrite = %v, want it to go through", err)
	}
	if got := readFile(t, file.path); got != string(edited) {
		t.Errorf("overwriting left %q", got)
	}
}

// pausedAtTheGateForReal drives a run until the gate stops it, with no
// terminal anywhere, so that its journal says what the gate did.
func pausedAtTheGateForReal(t *testing.T, plans ...string) (*runPlan, *state.Store) {
	t.Helper()
	var quiet bytes.Buffer
	u := &ui{out: &quiet, err: &quiet}
	p, store, _ := pausedRun(t, u, plans...)
	if err := driveRun(cmdWithContext(), u, p, store, false); err != nil {
		t.Fatalf("driving the run to its gate: %v", err)
	}
	return p, store
}

// TestPlanOpensTheViewerOnARunWaitingAtItsPlan: `belay plan` at a terminal
// is the way in to the viewer, and it shows the plan of the run it picked,
// in the mode that lets the person change it.
func TestPlanOpensTheViewerOnARunWaitingAtItsPlan(t *testing.T) {
	_, store := pausedAtTheGateForReal(t, firstPlan)
	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionLeave}}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	cmd := cmdWithContext()
	cmd.Flags().String("workspace", store.Layout().WorkspaceDir(), "")

	if err := showPlan(cmd, u, store.Layout().RunID()); err != nil {
		t.Fatalf("belay plan: %v", err)
	}
	if len(reviewer.opens) != 1 {
		t.Fatalf("the viewer was opened %d time(s), want once", len(reviewer.opens))
	}
	opened := reviewer.opens[0]
	if opened.Mode != planview.Review {
		t.Errorf("the viewer opened in mode %v, want the one that can change the plan", opened.Mode)
	}
	if string(opened.Plan) != firstPlan {
		t.Errorf("the viewer was given %q, want the plan on disk", opened.Plan)
	}
	if want := filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName); opened.Path != want {
		t.Errorf("the viewer was told the plan is at %q, want %q", opened.Path, want)
	}
	got := out.String()
	for _, want := range []string{"belay resume " + store.Layout().RunID(), "belay plan " + store.Layout().RunID()} {
		if !strings.Contains(got, want) {
			t.Errorf("leaving the viewer does not say how to come back (%q):\n%s", want, got)
		}
	}
}

// TestPlanShowsARunThatIsNotWaitingReadOnly: a run that is not stopped at
// its plan can still be read, but nothing in it may be changed, and the
// viewer is told to say why.
func TestPlanShowsARunThatIsNotWaitingReadOnly(t *testing.T) {
	store := pausedAtTheGate(t, regenPlanBody)
	ws := store.Layout().WorkspaceDir()
	seedManifest(t, ws, store.Layout().RunID(), state.RunStatusRunning, graph.NodeTest, "add tests", time.Now().UTC())

	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionLeave}}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	cmd := cmdWithContext()
	cmd.Flags().String("workspace", ws, "")

	if err := showPlan(cmd, u, store.Layout().RunID()); err != nil {
		t.Fatalf("belay plan: %v", err)
	}
	if len(reviewer.opens) != 1 {
		t.Fatalf("the viewer was opened %d time(s), want once", len(reviewer.opens))
	}
	opened := reviewer.opens[0]
	if opened.Mode != planview.ReadOnly {
		t.Errorf("the viewer opened in mode %v, want read-only", opened.Mode)
	}
	if !strings.Contains(opened.ReadOnlyReason, "test") {
		t.Errorf("the reason does not say where the run is: %q", opened.ReadOnlyReason)
	}
}

// TestPlanWithoutATerminalPrintsThePlanAsItAlwaysHas: the headless form is
// untouched, and it is what a run without a terminal still gets.
func TestPlanWithoutATerminalPrintsThePlanAsItAlwaysHas(t *testing.T) {
	_, store := pausedAtTheGateForReal(t, firstPlan)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out}
	u.review = func(context.Context, *os.File, *os.File, planview.Options) (planview.Decision, error) {
		t.Fatal("belay plan opened the viewer with no terminal to open it on")
		return planview.Decision{}, nil
	}
	cmd := cmdWithContext()
	cmd.Flags().String("workspace", store.Layout().WorkspaceDir(), "")

	if err := showPlan(cmd, u, store.Layout().RunID()); err != nil {
		t.Fatalf("belay plan: %v", err)
	}
	got := out.String()
	assertPlainText(t, "belay plan", got)
	for _, want := range []string{
		"1. Rewrite the parser.",
		filepath.Join(store.Layout().ArtifactsDir(), plan.ArtifactName),
		"Edit that file if you want to, then approve it and carry on with:",
		"belay resume " + store.Layout().RunID(),
		"--regenerate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("belay plan no longer says %q:\n%s", want, got)
		}
	}
}

// TestApprovingFromPlanContinuesTheRun: approving in the viewer opened by
// `belay plan` continues the run, through the same approval and the same
// dispatch as `belay resume`.
func TestApprovingFromPlanContinuesTheRun(t *testing.T) {
	p, store := pausedAtTheGateForReal(t, firstPlan)
	reviewer := &scriptedReviewer{answers: []planview.Decision{{Action: planview.ActionApprove}}}
	var out bytes.Buffer
	u := reviewUI(&out, reviewer)
	p.logger = newLogger(u)
	reached := false
	registry := graph.NewRegistry()
	registry.MustRegister(plan.New(), approve.New(), endsTheRun{reached: &reached})
	p.registry = registry
	chosen, err := chooseRun(store.Layout().WorkspaceDir(), store.Layout().RunID())
	if err != nil {
		t.Fatalf("chooseRun: %v", err)
	}

	if err := reviewPlanOfRun(cmdWithContext(), u, p, store, chosen, true); err != nil {
		t.Fatalf("reviewing the plan: %v", err)
	}
	if !reached {
		t.Error("approving in the viewer did not continue the run")
	}
	st, err := store.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !st.Plan.Approved {
		t.Error("the approval was not recorded")
	}
	if got := out.String(); !strings.Contains(got, "belay is continuing a run") {
		t.Errorf("belay did not say it was continuing the run:\n%s", got)
	}
}
