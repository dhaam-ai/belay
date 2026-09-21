package cli

import (
	"github.com/dhaam-ai/belay/internal/planarchive"
	"github.com/dhaam-ai/belay/internal/state"
)

// archivePlan moves a completed run's plan out of the run directory and says
// where it went. It is a step of afterCompletion, which says why it comes
// where it does, and like every step there it cannot change how the run is
// reported.
//
// internal/planarchive does the moving: in a run that edited its workspace
// in place it writes the plan into docs/plans (or wherever the run's
// manifest pinned) and removes the run's copy; in a run on a branch, whose
// promote node already put the plan in the run's commit, it confirms the
// plan is there and removes the run's copy. It is given the Store the run's
// nodes worked through, rebuilt from view.layout, because for a branch run
// that is the one pointed at the run's worktree.
//
// A plan that stays in the run directory is said on stderr, with the reason
// and where the plan is, because the person has to know their plan did not
// go where the run said it would. A run that never asked for archival, or
// did not complete, gets no line at all: its plan is where it always was.
func archivePlan(u *ui, view outcomeView) {
	if view.outcome.Status != state.RunStatusCompleted {
		return
	}
	res, err := planarchive.File(state.NewStore(view.layout))
	switch {
	case err != nil:
		u.note("belay: the plan stays in the run directory, at %s: belay could not move it: %v", orUnknown(res.Source), err)
	case res.Outcome == planarchive.Kept:
		u.note("belay: the plan stays in the run directory, at %s: %s", orUnknown(res.Source), res.Reason)
	case res.Outcome == planarchive.NotAsked:
	case view.output.Branched():
		u.say("  plan committed on %s, at %s", view.output.Branch, res.Rel)
	default:
		u.say("  plan moved to %s", res.Path)
	}
}

// orUnknown fills in for a path belay could not work out.
func orUnknown(path string) string {
	if path == "" {
		return "an unknown path"
	}
	return path
}
