// Package planarchive moves a finished run's approved plan out of the run's
// scratch directory and into the repository the run changed, so the record
// of why the code looks the way it does outlives .belay/runs/<run-id>/,
// which the README calls safe to delete. ADR-0016 records the decision.
//
// # The name
//
// An archived plan is <plan.archive_dir>/<run-id>-<slug>.md, relative to the
// tree the run worked in. The run id comes first, so a directory of them
// sorts by when each run started and every name begins with what `belay
// timeline` takes; the slug is the goal, reduced to what any file system and
// any shell accepts. Rel builds the path, and it is the one place that does:
// the promote node, which writes a branch run's plan into its commit, and
// the CLI, which moves an in-place run's plan after the run, must agree on
// the name to the byte, or the second would not find what the first wrote.
//
// # Two writers, one rule
//
// Place is the only way this package writes into a repository, and it never
// replaces anything, never leaves the workspace and never leaves a torn
// file. File is the move: it places the plan, records in the run's manifest
// where it went, and only then removes the run's copy, so a crash anywhere
// leaves the plan whole in at least one place and a re-run finishes the job
// instead of filing it twice. Approved decides which bytes are the plan —
// the ones the run's code was written from — by the approve gate's own
// rules, for both writers.
//
// # What it does not do
//
// It never runs git. A run that edits its workspace in place leaves the
// archived plan as a new file beside its change, for the person to commit
// or delete; a run on a branch gets the plan into its commit through the
// promote node, which commits everything in the worktree anyway. It does
// not rewrite state.json's Plan.Path, which goes on naming the artifact the
// plan was moved out of: the manifest's pointer supersedes it, and the
// journal and the blackboard are left exactly as the run left them, which
// is what keeps `belay timeline` unchanged. It moves nothing else out of the
// run directory: the plans a regeneration replaced, the diffs and the
// agent's transcripts stay with the run.
package planarchive
