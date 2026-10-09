//go:build fixrate

package fixrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file makes SWE-bench's own harness the scoring authority for every
// SWE-bench cell, and keeps this package's port of its grading as the
// cross-check.
//
// # Why the official harness, and not the port
//
// swebench.go ports SWE-bench's grading rule, and a port can drift. More to
// the point, grading is the last step of evaluation, not all of it: the
// official harness also builds the instance's container, applies the patch
// the way every submission's patch is applied, resets the test files and
// applies the dataset's test patch, runs the tests, and parses the log with
// the repository's own log parser. A resolve rate that is to be read beside
// anyone else's has to come out of that pipeline. So Evaluate runs it, and
// the verdict it writes is the score.
//
// # How it is run, from the source
//
// Everything below was read from swebench/harness/run_evaluation.py, utils.py
// and constants/__init__.py as the swebench 5.0.2 release installs them from
// PyPI -- the package the live command requires -- and from the evaluation
// guide, docs/guides/evaluation.md. Upstream's main branch
// (02e7a74ffd0b707aab73d203fe87bdc7c76afc8e) still calls itself 5.0.2 but
// differs from the release in two places, both about where files go: its
// RUN_EVALUATION_LOG_DIR is logs/evaluation where the release's is
// logs/run_evaluation, and it has dropped --report_dir. utils.py and
// grading.py are identical in both.
//
//   - the entry point is `python -m swebench.harness.run_evaluation`, with
//     --dataset_name, --split, --instance_ids, --predictions_path,
//     --max_workers, --run_id and --timeout;
//   - --dataset_name may be "a path to JSON file": load_swebench_dataset reads
//     a local .json, .jsonl or .parquet, so Evaluate passes the very file the
//     tasks were read from, and the harness grades against exactly the
//     records the arms were given;
//   - a prediction is a record carrying instance_id, model_name_or_path and
//     model_patch;
//   - each instance's result lands in
//     <RUN_EVALUATION_LOG_DIR>/<run_id>/<model_name_or_path>/<instance_id>/report.json
//     under the harness's working directory, with "/" in the model name
//     written "__"; report.json maps the instance id to resolved,
//     patch_successfully_applied, infra_failure and tests_status. Because
//     that directory is the one thing the release and main disagree on,
//     Evaluate asks the installed harness for its RUN_EVALUATION_LOG_DIR
//     rather than assuming either: a report looked for in the wrong place is
//     never found, and every cell would read as one the harness could not
//     grade;
//   - an instance with no report.json is one the harness could not grade --
//     "a patch that did not apply, a container that did not start, or a run
//     that timed out" -- and run_instance.log says which;
//   - an empty model_patch is left out of the run altogether and counted as an
//     empty patch, so Evaluate does not start the harness for one;
//   - results are cached by run id, model name and instance: a report that
//     exists is returned as it is. A cell is therefore never graded twice,
//     and each arm and replicate must be its own model name, which is what
//     Cell.System is for.
//
// One harness process per cell, with one instance and one worker, keeps a
// cell's verdict independent of every other cell's and makes it resumable on
// its own. It costs a process start per cell, against an evaluation that
// takes minutes.
//
// # The cross-check, and what a disagreement means
//
// report.json carries the harness's verdict and the per-test lists it was
// reached from, but not the raw test statuses. The cross-check asks the
// harness's own log reader for them -- get_logs_eval over the same
// test_output.txt and the same record, which is exactly what the harness
// graded -- and grades them with Task.Resolved. Two implementations of the
// rule over one input: when they disagree, either the port has drifted, or
// the instance meets one of the port's three deliberate departures
// (swebench.go). Either way the official verdict is the score, unchanged, and
// the disagreement is recorded on the verdict so the report can count it
// and say so. Nothing averages the two.
//
// The helper calls get_logs_eval and make_test_spec, which are internal to
// the harness rather than its documented interface. If a later swebench moves
// them, the cross-check reports itself unavailable for every cell -- loudly,
// in the report -- and the score is untouched.

// SWEBenchHarnessVersion is the swebench release this file was written
// against and the live command requires, so the scoring authority cannot
// change underneath a corpus.
const SWEBenchHarnessVersion = "5.0.2"

// sweBenchStatusScript asks the harness's own log reader for the raw test
// statuses it graded one instance by. See this file's "The cross-check".
const sweBenchStatusScript = `import json, sys
from swebench.harness.grading import get_logs_eval
from swebench.harness.utils import load_swebench_dataset, make_test_spec

dataset, instance_id, test_log = sys.argv[1:4]
instance = load_swebench_dataset(dataset, "test", [instance_id])[0]
status_map, found = get_logs_eval(make_test_spec(instance), test_log)
json.dump({"found": found, "status_map": status_map}, sys.stdout)
`

// sweBenchLogDirScript prints the directory, relative to the harness's
// working directory, that the installed harness writes evaluation logs under.
const sweBenchLogDirScript = "from swebench.harness.constants import RUN_EVALUATION_LOG_DIR\nprint(RUN_EVALUATION_LOG_DIR)\n"

// errHarnessConfig reports a Harness or an Evaluate call missing something it
// needs. It is a wiring bug, not an evaluation outcome.
var errHarnessConfig = errors.New("fixrate: unusable harness configuration")

// pathSegment matches a name this file is willing to make a directory or file
// from: an instance id, a run id or a system name.
var pathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Harness runs SWE-bench's official evaluation, one prediction at a time. See
// this file's header comment.
type Harness struct {
	// Python is the interpreter swebench is installed under. Required. Its
	// Env reaches the harness process, for the harness's own settings such
	// as SWEBENCH_DOCKER_TIMEOUT.
	Python Python
	// Dataset is the JSON-lines file the corpus's tasks were read from, as
	// an absolute path. The harness grades against exactly these records.
	Dataset string
	// Dir is the absolute directory the harness runs in. Its logs/evaluation
	// tree, the predictions files and this file's own logs land there.
	Dir string
	// RunID names the evaluation run; see the caching note in this file's
	// header comment.
	RunID string
	// Timeout bounds one instance's tests, passed to --timeout in whole
	// seconds. Zero leaves the harness's own default, 1800 seconds.
	Timeout time.Duration
}

// Prediction is one record of the predictions file run_evaluation reads.
type Prediction struct {
	InstanceID      string `json:"instance_id"`
	ModelNameOrPath string `json:"model_name_or_path"`
	ModelPatch      string `json:"model_patch"`
}

// Verdict is the official harness's verdict on one prediction, with the
// port's cross-check beside it.
type Verdict struct {
	// Evaluated reports whether the harness wrote a report for the
	// prediction. One it could not grade did not resolve.
	Evaluated bool `json:"evaluated"`
	// Resolved is the official verdict, SWE-bench's RESOLVED_FULL, and the
	// only thing a resolve rate counts. False whenever Evaluated is.
	Resolved bool `json:"resolved"`
	// EmptyPatch reports a prediction with nothing in it, which the harness
	// does not run and which cannot resolve.
	EmptyPatch bool `json:"empty_patch,omitempty"`
	// PatchApplied is the report's patch_successfully_applied: whether the
	// harness found test output it could grade.
	PatchApplied bool `json:"patch_applied"`
	// InfraFailure is the report's infra_failure, the harness's own advisory
	// that a failure looks environmental rather than the patch's doing.
	InfraFailure bool `json:"infra_failure,omitempty"`
	// Error is the harness's own account of a prediction it could not grade
	// or flagged as an infrastructure failure, from its logs.
	Error string `json:"error,omitempty"`
	// Port is the cross-check.
	Port PortCheck `json:"port"`
}

// PortCheck is Task.Resolved's verdict on the test statuses the official
// harness graded. See this file's "The cross-check".
type PortCheck struct {
	// Checked reports whether the port graded the statuses at all.
	Checked bool `json:"checked"`
	// Resolved is the port's verdict, meaningful only when Checked.
	Resolved bool `json:"resolved"`
	// Reason is the port's account of its verdict.
	Reason string `json:"reason,omitempty"`
	// Disagrees reports a checked verdict that differs from the official
	// one. The official verdict stands; this is how the report knows to
	// say so.
	Disagrees bool `json:"disagrees,omitempty"`
	// Unavailable says why the port could not check, when Checked is false
	// although the harness wrote a report.
	Unavailable string `json:"unavailable,omitempty"`
}

// harnessReport is one instance's entry in report.json, with only the fields
// Evaluate reads.
type harnessReport struct {
	PatchSuccessfullyApplied bool   `json:"patch_successfully_applied"`
	Resolved                 bool   `json:"resolved"`
	InfraFailure             bool   `json:"infra_failure"`
	InfraFailureReason       string `json:"infra_failure_reason"`
}

// validate refuses a Harness, an instance id or a system name Evaluate cannot
// safely use.
func (h Harness) validate(instanceID, system string) error {
	switch {
	case h.Python.Path == "":
		return fmt.Errorf("%w: no Python interpreter", errHarnessConfig)
	case !filepath.IsAbs(h.Dataset):
		return fmt.Errorf("%w: the dataset %q is not an absolute path", errHarnessConfig, h.Dataset)
	case !filepath.IsAbs(h.Dir):
		return fmt.Errorf("%w: the directory %q is not an absolute path", errHarnessConfig, h.Dir)
	case !pathSegment.MatchString(h.RunID):
		return fmt.Errorf("%w: run id %q", errHarnessConfig, h.RunID)
	case !pathSegment.MatchString(instanceID):
		return fmt.Errorf("%w: instance id %q cannot name a file", errHarnessConfig, instanceID)
	case !pathSegment.MatchString(system):
		return fmt.Errorf("%w: system name %q cannot name a directory", errHarnessConfig, system)
	default:
		return nil
	}
}

// Evaluate runs the official harness on one prediction -- task's instance,
// patched with patch, under the model name system -- and returns its verdict
// with the port's cross-check. See this file's header comment.
//
// The error return is for a harness that could not run at all: a bad
// configuration, a process that failed to start or exited non-zero, a report
// it could not have written. A prediction the harness ran and could not
// grade is a Verdict, not an error. A cancelled ctx is returned as an error
// too, so a caller never records a verdict an interruption cut short.
func (h Harness) Evaluate(ctx context.Context, task Task, system, patch string) (Verdict, error) {
	if err := h.validate(task.ID, system); err != nil {
		return Verdict{}, err
	}
	if strings.TrimSpace(patch) == "" {
		return Verdict{EmptyPatch: true}, nil
	}
	logRoot, err := h.logDir(ctx)
	if err != nil {
		return Verdict{}, err
	}

	preds := filepath.Join(h.Dir, "predictions", system, task.ID+".jsonl")
	if err := writePrediction(preds, Prediction{InstanceID: task.ID, ModelNameOrPath: system, ModelPatch: patch}); err != nil {
		return Verdict{}, err
	}
	args := []string{
		"-m", "swebench.harness.run_evaluation",
		"--dataset_name", h.Dataset, "--split", "test", "--instance_ids", task.ID,
		"--predictions_path", preds, "--max_workers", "1", "--run_id", h.RunID,
	}
	if h.Timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(int(math.Ceil(h.Timeout.Seconds()))))
	}
	logPath := filepath.Join(h.Dir, "harness-logs", system, task.ID+".log")
	if err := h.run(ctx, logPath, args...); err != nil {
		return Verdict{}, fmt.Errorf("fixrate: the official harness could not evaluate %s for %s: %w; see %s",
			task.ID, system, err, logPath)
	}

	instDir := filepath.Join(logRoot, h.RunID, strings.ReplaceAll(system, "/", "__"), task.ID)
	data, err := os.ReadFile(filepath.Join(instDir, "report.json")) //nolint:gosec // composed from validated segments under Harness.Dir
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Verdict{Error: whyNoReport(instDir)}, nil
	case err != nil:
		return Verdict{}, fmt.Errorf("fixrate: reading the harness's report for %s: %w", task.ID, err)
	}
	var doc map[string]harnessReport
	if err := json.Unmarshal(data, &doc); err != nil {
		return Verdict{}, fmt.Errorf("fixrate: the harness's report for %s is not the JSON it writes: %w", task.ID, err)
	}
	entry, ok := doc[task.ID]
	if !ok {
		return Verdict{}, fmt.Errorf("fixrate: the harness's report for %s has no entry for it", task.ID)
	}

	v := Verdict{
		Evaluated:    true,
		Resolved:     entry.Resolved,
		PatchApplied: entry.PatchSuccessfullyApplied,
		InfraFailure: entry.InfraFailure,
		Error:        entry.InfraFailureReason,
	}
	v.Port = h.crossCheck(ctx, task, filepath.Join(instDir, "test_output.txt"), v.Resolved)
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	return v, nil
}

// logDir asks the installed harness where it writes evaluation logs, and
// returns that directory under Harness.Dir. See this file's header comment
// for why it asks.
func (h Harness) logDir(ctx context.Context) (string, error) {
	out, err := h.Python.output(ctx, "-c", sweBenchLogDirScript)
	if err != nil {
		return "", fmt.Errorf("fixrate: asking the installed harness where it writes its logs: %w", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" || filepath.IsAbs(dir) || strings.Contains(dir, "..") {
		return "", fmt.Errorf("fixrate: the installed harness names %q as its log directory, which is not a path under its working directory", dir)
	}
	return filepath.Join(h.Dir, filepath.FromSlash(dir)), nil
}

// writePrediction writes p as the one record of a predictions file at path.
func writePrediction(path string, p Prediction) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("fixrate: create %s: %w", filepath.Dir(path), err)
	}
	line, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("fixrate: encode the prediction for %s: %w", p.InstanceID, err)
	}
	return writeFileAtomic(path, append(line, '\n'), 0o600)
}

// run runs Python with args in Harness.Dir, writing everything it prints to
// logPath.
func (h Harness) run(ctx context.Context, logPath string, args ...string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(logPath), err)
	}
	log, err := os.Create(logPath) //nolint:gosec // composed from validated segments under Harness.Dir
	if err != nil {
		return fmt.Errorf("create %s: %w", logPath, err)
	}
	cmd := exec.CommandContext(ctx, h.Python.Path, args...) //nolint:gosec // the configured interpreter, with this file's own arguments
	cmd.Dir = h.Dir
	cmd.Env = append(os.Environ(), h.Python.Env...)
	cmd.Stdout = log
	cmd.Stderr = log
	runErr := cmd.Run()
	if err := errors.Join(runErr, log.Close()); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(ctxErr, err)
		}
		return err
	}
	return nil
}

// crossCheck grades the statuses the harness graded with Task.Resolved; see
// this file's "The cross-check".
func (h Harness) crossCheck(ctx context.Context, task Task, testLog string, official bool) PortCheck {
	out, err := h.Python.output(ctx, "-c", sweBenchStatusScript, h.Dataset, task.ID, testLog)
	if err != nil {
		return PortCheck{Unavailable: fmt.Sprintf("the harness's log reader could not be run: %v", err)}
	}
	var answer struct {
		Found     *bool             `json:"found"`
		StatusMap map[string]string `json:"status_map"`
	}
	if err := json.Unmarshal(out, &answer); err != nil || answer.Found == nil {
		return PortCheck{Unavailable: fmt.Sprintf("the harness's log reader printed something other than its answer: %q",
			lastLines(string(out), 3))}
	}
	if !*answer.Found {
		return PortCheck{
			Checked:   true,
			Reason:    "the harness found no test output it could grade, so nothing resolved",
			Disagrees: official,
		}
	}
	resolved, reason := task.Resolved(answer.StatusMap)
	return PortCheck{Checked: true, Resolved: resolved, Reason: reason, Disagrees: resolved != official}
}

// whyNoReport is the harness's own account of an instance it wrote no report
// for: the end of its run_instance.log, where it says why it stopped.
func whyNoReport(instDir string) string {
	data, err := os.ReadFile(filepath.Join(instDir, "run_instance.log")) //nolint:gosec // under Harness.Dir
	if err != nil {
		return "the harness wrote no report.json and no run_instance.log for this instance"
	}
	return "the harness wrote no report.json; run_instance.log ends: " + lastLines(string(data), 3)
}

// lastLines returns the last n non-empty lines of s, joined with " | ".
func lastLines(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, " | ")
}
