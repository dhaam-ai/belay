package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/planview"
	"github.com/dhaam-ai/belay/internal/state"
)

// reviewer opens the plan viewer and returns what the person decided.
// planview.Run is the one belay uses; a test replaces it.
type reviewer func(ctx context.Context, in, out *os.File, opts planview.Options) (planview.Decision, error)

// reviewPause offers the plan viewer to somebody whose run has just
// stopped at the approval gate, and carries out what they decided. It
// reports whether the run should be dispatched again.
//
// It runs between two dispatches, and that is the whole design. The gate
// has already paused the run and the dispatcher has already journalled it,
// so the viewer opens on a run that is durably waiting; nothing here runs
// a node, and the viewer is closed before the next dispatch begins. Kill
// belay at any instant and the run is exactly where the gate left it. The
// approve node therefore never reads a terminal, and a paused run still
// outlives the session that started it.
//
// Nothing it does is a second way into the graph. Approving is what `belay
// resume` does, recordApproval, and asking for a new plan is what `belay
// plan --regenerate` does, recordRegeneration. The viewer's own writing is
// the plan file and nothing else.
//
// Every refusal here — no terminal, a viewer that could not open, a person
// who left — ends in the same place: false, and the caller reports the
// pause as belay always has.
func reviewPause(cmd *cobra.Command, u *ui, p *runPlan, store *state.Store, view outcomeView) bool {
	if !u.mayReview() || !askedAboutThePlan(view, store) {
		return false
	}
	opts, err := planOptions(u, p, store)
	if err != nil {
		u.note("belay: the plan cannot be reviewed here: %v", err)
		return false
	}
	decision, err := u.review(cmd.Context(), u.viewer.in, u.viewer.out, opts)
	if err != nil {
		u.note("belay: the plan viewer stopped: %v", err)
		return false
	}
	switch decision.Action {
	case planview.ActionApprove:
		if err := recordApproval(u, store); err != nil {
			u.note("belay: %v", err)
			return false
		}
		return true
	case planview.ActionRegenerate:
		if err := recordRegeneration(u, store, decision.Guidance); err != nil {
			u.note("belay: %v", err)
			return false
		}
		return true
	default:
		return false
	}
}

// mayReview reports whether this invocation may open the plan viewer at
// all: there is a terminal to open it on, and something to open.
func (u *ui) mayReview() bool { return u.viewer.ok && u.review != nil }

// askedAboutThePlan reports whether this dispatch ended with the gate
// stopping the run to ask about its plan.
//
// It is not enough that the run is paused at the approve node. A Ctrl-C
// while the gate ran, and a limit belay stopped itself at before the gate
// could ask, both leave the same manifest; in neither case has anybody
// been shown a plan, and resuming must run the gate rather than approve on
// their behalf. The journal is what tells them apart, which is what
// awaitsApproval reads for `belay resume`.
func askedAboutThePlan(view outcomeView, store *state.Store) bool {
	if view.runErr != nil || view.outcome.Status != state.RunStatusPaused || view.outcome.Node != graph.NodeApprove {
		return false
	}
	asked, err := gateAsked(store.Layout())
	return err == nil && asked
}

// planOptions is everything the viewer is shown: the run, its goal, the
// plan as it is on disk, what the run has spent, and what it may not do.
func planOptions(u *ui, p *runPlan, store *state.Store) (planview.Options, error) {
	file, err := openPlanFile(store)
	if err != nil {
		return planview.Options{}, err
	}
	opts := planview.Options{
		RunID:   store.Layout().RunID(),
		Goal:    p.goal,
		Path:    file.path,
		Plan:    file.loaded,
		Mode:    planview.Review,
		Save:    file.save,
		Reload:  file.reload,
		Blocker: blockersSentence(p),
		Color:   u.color,
		ASCII:   u.viewer.ascii,
		Locale:  u.viewer.locale,
	}
	manifest, err := store.LoadManifest()
	if err == nil {
		if opts.Goal == "" {
			opts.Goal = manifest.Goal
		}
		opts.Spend = planview.Spend{
			SpentUSD:  manifest.Budget.SpentUSD,
			LimitUSD:  p.config.Budget.MaxUSD,
			Estimated: manifest.Budget.Estimated,
		}
	}
	return opts, nil
}

// blockersSentence is why this run cannot continue from here, in one piece of
// prose, or "" when it can. The viewer refuses to approve or to ask for a
// new plan while it is set: both dispatch the run, and a run belay would
// refuse to start must not be approved only to stop again.
func blockersSentence(p *runPlan) string {
	if len(p.blockers) == 0 {
		return ""
	}
	said := make([]string, 0, len(p.blockers))
	for _, b := range p.blockers {
		said = append(said, b.problem+" To fix it: "+b.fix)
	}
	return strings.Join(said, " ")
}

// recordApproval records that the person approved the plan, and says what
// that changed. It is what `belay resume` does, and the only way an
// approval is recorded.
func recordApproval(u *ui, store *state.Store) error {
	// The approval is the first thing belay writes to this run, and
	// Preflight refuses a run this belay may not read and upgrades an older
	// manifest before anything else is written (ADR-0011).
	if err := store.Preflight(); err != nil {
		return err
	}
	done, err := approvePlan(store)
	if err != nil {
		return err
	}
	if done.recorded {
		u.say("  the plan is approved; belay reads it again now, so any edit you made counts")
	}
	if done.withdrew {
		u.say("  the new plan you asked for is no longer wanted, so belay will not write one")
	}
	if done.changed() {
		u.say("")
	}
	return nil
}

// recordRegeneration records a request for a new plan, and says what
// happens next. It is what `belay plan --regenerate` does, and the only way
// a request is recorded.
func recordRegeneration(u *ui, store *state.Store, guidance string) error {
	if err := store.Preflight(); err != nil {
		return err
	}
	if err := requestRegeneration(store, guidance); err != nil {
		return err
	}
	u.say("  belay is writing a new plan; the run stops at the plan again when it has one")
	u.say("")
	return nil
}

// planFile is a paused run's plan artifact, as the viewer reads and writes
// it: the file the approve gate will read, found the gate's own way.
type planFile struct {
	store *state.Store
	// name is the artifact name the run records, and path the file itself.
	name string
	path string
	// loaded is what the file held when this process last read it or wrote
	// it, which is how an edit made elsewhere is noticed.
	loaded []byte
}

// openPlanFile locates and reads the plan of a paused run.
func openPlanFile(store *state.Store) (*planFile, error) {
	st, err := store.LoadState()
	if err != nil {
		return nil, fmt.Errorf("cannot read this run's state: %w", err)
	}
	name, abs, err := approve.PlanArtifact(store.Layout(), st.Plan.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs) //nolint:gosec // path is constrained by approve.PlanArtifact
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", abs, err)
	}
	return &planFile{store: store, name: name, path: abs, loaded: data}, nil
}

// save writes plan to the plan file, atomically, and refuses two things
// first.
//
// The gate's own rule, approve.CheckPlan, decides whether these bytes may
// be saved at all, so the viewer can never write a plan the gate would
// then refuse to let the run past — and belay has one rule about that
// rather than two that could drift apart.
//
// And unless overwrite says otherwise, a file that no longer holds what
// this process last read or wrote is not written over: somebody may have
// the plan open in their own editor, which is how plans were reviewed
// before the viewer existed. The viewer offers them the choice.
func (f *planFile) save(plan []byte, overwrite bool) error {
	if err := approve.CheckPlan(plan); err != nil {
		return err
	}
	if !overwrite {
		onDisk, err := os.ReadFile(f.path) //nolint:gosec // as in openPlanFile
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", f.path, err)
		}
		if !bytes.Equal(onDisk, f.loaded) {
			return planview.ErrChangedOnDisk
		}
	}
	// state.ReplaceArtifact rather than a plain write: between this and the
	// next dispatch there is a paused run on disk whose state.json names
	// this file, and a person who may open it at any moment.
	if err := state.ReplaceArtifact(f.store.Layout(), f.name, plan); err != nil {
		return err
	}
	f.loaded = bytes.Clone(plan)
	return nil
}

// reload reads the plan file again, and takes what it reads as what the
// file now holds.
func (f *planFile) reload() ([]byte, error) {
	data, err := os.ReadFile(f.path) //nolint:gosec // as in openPlanFile
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", f.path, err)
	}
	f.loaded = data
	return data, nil
}
