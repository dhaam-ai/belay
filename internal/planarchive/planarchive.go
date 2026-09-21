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
package planarchive
