//go:build fixrate

package fixrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// This file is the offline SWE-bench adapter: it turns SWE-bench records
// into Tasks both measurement arms can be handed, and ports SWE-bench's
// own rule for deciding whether a task was resolved. It runs nothing. It
// builds no container, clones no repository, applies no patch and touches
// no network; containers, the corpus runner and the live command are the
// next slice's work (6c). Everything below that is a claim about
// SWE-bench is cited to a primary source, and everything that could not
// be confirmed is marked UNKNOWN rather than filled in.
//
// # The pin
//
// The corpus is SWE-bench/SWE-bench_Verified at revision
// 78f471bf655a3137b2e8a75af1501690ec009ec3 (SWEBenchDataset,
// SWEBenchRevision), the revision this roadmap settled on. On 2026-09-30
// that sha was the dataset repository's HEAD
// (https://huggingface.co/api/datasets/SWE-bench/SWE-bench_Verified), with
// one split, "test", of 500 instances
// (https://datasets-server.huggingface.co/info?dataset=SWE-bench%2FSWE-bench_Verified).
// The same sha does not exist on princeton-nlp/SWE-bench_Verified, which
// is a different repository with a different history.
//
// # The schema, and an encoding drift that is real
//
// At the pin a record has exactly 17 fields, and Instance's JSON tags are
// those names verbatim, guarded by TestInstance_FieldNamesMatchThePinnedSchema:
// instance_id, repo, base_commit, environment_setup_commit,
// problem_statement, patch, test_patch, hints_text, created_at, version,
// difficulty, eval_script, eval_type, image, log_parser, FAIL_TO_PASS and
// PASS_TO_PASS (source: the datasets-server feature list above). The older
// princeton-nlp/SWE-bench (12 fields) and princeton-nlp/SWE-bench_Verified
// (13 fields) carry subsets of them.
//
// FAIL_TO_PASS and PASS_TO_PASS are encoded DIFFERENTLY across those
// copies. At the pin they are JSON arrays of strings. In both princeton-nlp
// copies they are strings holding a JSON-encoded list, which is also how
// that dataset card's prose describes them: "A json list of strings
// representing the set of tests resolved by the PR"
// (https://huggingface.co/datasets/princeton-nlp/SWE-bench_Verified).
// TestNames reads both. Either would be easy to get wrong without an
// error, and getting it wrong is the expensive direction: a list that
// silently decodes empty is a task that resolves for free (see below).
//
// # How an instance is judged
//
// Task.Resolved ports SWE-bench's own grading, read from
// swebench/harness/grading.py at commit
// ca6e4e0d252f32f8762625b73575d5dee49d0a5a, the last change to that file
// as of upstream main 02e7a74ffd0b707aab73d203fe87bdc7c76afc8e
// (https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/swebench/harness/grading.py).
// For an instance whose eval_type is "pass_and_fail":
//
//   - a FAIL_TO_PASS test succeeds when its status is PASSED or XFAIL
//     (test_passed), and fails when it is FAILED, ERROR, SKIPPED, or absent
//     (test_failed);
//   - a PASS_TO_PASS test is held to test_maintained instead, under which
//     SKIPPED also succeeds -- "a skipped test is not a regression, unlike
//     for F2P", in the function's own docstring;
//   - an expected test name is looked up through _resolve_case, which falls
//     back to prefix matching for a name with more '[' than ']', because
//     "676 expected ids in SWE-bench_Verified are truncated mid-parameter
//     (issue #290)", and accepts the matches only when they all agree on
//     pass-versus-fail;
//   - a task is FULLY resolved when every FAIL_TO_PASS test and every
//     PASS_TO_PASS test succeeds (get_resolution_status). RESOLVED_PARTIAL
//     -- some FAIL_TO_PASS tests passing, none regressing -- is NOT
//     resolved here, as it is not in a published resolve rate.
//
// The status values are SWE-bench's TestStatus enum (FAILED, PASSED,
// SKIPPED, ERROR, XFAIL) and the eval types its EvalType enum
// ("pass_and_fail", "fail_only"), both from
// https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/swebench/harness/constants/__init__.py.
//
// Only "pass_and_fail" is ported. "fail_only" grades differently -- only
// an explicit FAILED fails, matched by exact key, so an absent test counts
// as a success -- and the pinned revision contains no instance of it: its
// eval_type column is "pass_and_fail" for all 500 records, per the
// datasets-server's aggregate statistics
// (https://datasets-server.huggingface.co/statistics?dataset=SWE-bench%2FSWE-bench_Verified&config=default&split=test).
// A "fail_only" instance is refused rather than graded by the wrong rule.
// A record with no eval_type at all (the princeton-nlp copies) gets
// get_eval_tests_report's own default, which is "pass_and_fail".
//
// # Where this port departs from upstream, and why each one is safe
//
// Three places, each closing a way a task could resolve without anything
// having been fixed. None of them can turn an unresolved task into a
// resolved one.
//
//  1. An empty FAIL_TO_PASS set. Upstream's compute_fail_to_pass returns 1
//     for an empty set, so such an instance resolves trivially. It would
//     inflate both arms' resolve rates identically and measure nothing.
//     Validate refuses the instance and Resolved refuses the task.
//  2. A status outside the TestStatus enum. Upstream drops such a test from
//     both its success and its failure lists, which only ever shrinks the
//     denominator -- a FAIL_TO_PASS set of nothing but unknown statuses
//     scores 1, the same hole as (1). SWE-bench's own log parsers never
//     emit one, so this only matters for a status map from somewhere else,
//     and there Resolved declines to vouch for any verdict.
//  3. One order-dependent corner of _resolve_case. When a truncated
//     PASS_TO_PASS name prefix-matches several tests that agree on "not
//     passing" but mix SKIPPED with FAILED or ERROR, upstream takes the
//     first match in log order, and test_maintained then says yes for
//     SKIPPED and no for the others. A status map has no log order, so the
//     port credits maintenance only when every match is SKIPPED -- the one
//     case in which every order gives the same answer.
//
// Because this is a port, it can drift from upstream. For a number that is
// going to be published, the official harness's own per-instance verdict
// is the authority, and this port is the cross-check that makes a
// disagreement between the two visible rather than the thing published.
//
// # The official harness needs Docker; this file builds nothing
//
// "SWE-bench uses Docker for reproducible evaluations"
// (https://github.com/SWE-bench/SWE-bench, README), and its evaluation
// guide says "The evaluation is performed in a containerized Docker
// environment to ensure consistent results across different platforms"
// (https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/docs/guides/evaluation.md).
// Its entry point is `python -m swebench.harness.run_evaluation`, taking
// --dataset_name, --predictions_path, --max_workers and --run_id, over
// predictions whose records carry instance_id, model_name_or_path and
// model_patch (same guide). Producing the status map Resolved consumes is
// therefore a container job, and it is 6c's.
//
// UNKNOWN: whether the official evaluation resets the test files touched
// by an instance's test_patch to base_commit before applying it. If it
// does, a regression test the goal asks an agent to write in one of those
// files is discarded before grading -- in both arms alike, so it cannot
// bias the delta, but it does mean the regression test is not part of what
// is graded. The module that builds the evaluation script could not be
// located to confirm it.
//
// # Licence
//
// The SWE-bench CODE is MIT licensed, "Copyright (c) 2023 Carlos E
// Jimenez, John Yang, Alexander Wettig, Shunyu Yao, Kexin Pei, Ofir Press,
// Karthik R Narasimhan"
// (https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/LICENSE).
// That licence covers the harness source this file ports from; it does
// not, by itself, cover the task instances.
//
// The DATASET's licence is UNKNOWN. As of 2026-09-30 none of
// SWE-bench/SWE-bench_Verified, princeton-nlp/SWE-bench_Verified or
// princeton-nlp/SWE-bench declares one: each repository's Hugging Face
// metadata has no license field and no license:* tag
// (https://huggingface.co/api/datasets/<repo>). Until it is established,
// this repository commits no dataset content -- every fixture under
// testdata/swebench is synthetic, which TestSWEBenchFixturesAreSynthetic
// guards -- and only aggregate results may be published.
//
// # What a number produced this way can claim, and what it cannot
//
// A resolve rate over Tasks from this adapter can claim exactly one thing:
// the share of the corpus whose dataset-supplied tests went from failing to
// passing without breaking the tests that passed before, judged by
// SWE-bench's own pass_and_fail rule. It cannot claim:
//
//   - that a fix is right. The tests are the dataset's; a change can pass
//     them and still be wrong, narrow, or unacceptable to the project's
//     maintainers, and no check of this shape can see that;
//   - comparability with a published SWE-bench leaderboard figure, unless
//     the status maps came from the official harness's own containers. Even
//     then, the goal text this adapter writes differs from any given
//     submission's prompt, and a resolve rate is a property of the agent,
//     the prompt and the harness together;
//   - anything at all in scripted mode. A ModeReplay agent does not read an
//     issue; a number from it over these tasks measures belay's plumbing,
//     exactly as Mode.provenance says;
//   - that the records came from the pinned revision. Source is the
//     caller's declaration, and this adapter cannot verify offline which
//     revision a record came from. Verifying it belongs to whatever
//     downloads the records (6c).

// SWEBenchDataset and SWEBenchRevision are the dataset and the exact
// revision this roadmap pins the SWE-bench corpus to. See this file's
// "The pin" section.
const (
	SWEBenchDataset  = "SWE-bench/SWE-bench_Verified"
	SWEBenchRevision = "78f471bf655a3137b2e8a75af1501690ec009ec3"
)

// sweBenchGradingRef pins the upstream source Task.Resolved ports, so a
// task's provenance names the exact rule its verdict was reached by.
const sweBenchGradingRef = "SWE-bench grading.py@ca6e4e0d252f32f8762625b73575d5dee49d0a5a"

// The SWE-bench TestStatus values a status map passed to Task.Resolved
// holds, spelled exactly as SWE-bench's TestStatus enum spells them.
const (
	SWEBenchPassed  = "PASSED"
	SWEBenchFailed  = "FAILED"
	SWEBenchSkipped = "SKIPPED"
	SWEBenchError   = "ERROR"
	SWEBenchXFail   = "XFAIL"
)

// sweBenchPassAndFail is the one EvalType value this file ports.
const sweBenchPassAndFail = "pass_and_fail"

// ErrInvalidInstance reports a SWE-bench record this adapter refuses to
// turn into a Task: one missing a field a task cannot be run without, one
// whose FAIL_TO_PASS set is empty, or one graded by an eval_type that is
// not ported.
var ErrInvalidInstance = errors.New("fixrate: unusable SWE-bench instance")

// ErrInvalidSource reports a Source that does not name a dataset and an
// exact commit sha.
var ErrInvalidSource = errors.New("fixrate: unusable SWE-bench source")

// Source names where a set of instances came from: a dataset and the exact
// revision of it.
//
// It is passed to Instance.Task by the caller, rather than stamped on by
// the adapter, because the adapter cannot know. Nothing in a record says
// which revision it was read from, and a label the adapter applied by
// default would be a provenance claim nobody made. This way the claim is a
// visible line in the caller's code.
type Source struct {
	Dataset  string `json:"dataset"`
	Revision string `json:"revision"`
}

// PinnedSource is the source this roadmap pins: SWEBenchDataset at
// SWEBenchRevision.
var PinnedSource = Source{Dataset: SWEBenchDataset, Revision: SWEBenchRevision}

// String renders s as "dataset@revision".
func (s Source) String() string { return s.Dataset + "@" + s.Revision }

// Validate reports whether s names a dataset and a full, lowercase,
// 40-character commit sha.
//
// A branch name such as "main" is refused because it moves: a number
// labelled with it cannot be traced back to the records that produced it,
// which is the only thing a source label is for. An abbreviated sha is
// refused for the same reason in a weaker form -- it is unambiguous today
// and need not stay so.
func (s Source) Validate() error {
	switch {
	case strings.TrimSpace(s.Dataset) == "":
		return fmt.Errorf("%w: no dataset named", ErrInvalidSource)
	case !isCommitSHA(s.Revision):
		return fmt.Errorf("%w: revision %q of %s is not a full 40-character commit sha", ErrInvalidSource, s.Revision, s.Dataset)
	default:
		return nil
	}
}

// isCommitSHA reports whether s is 40 lowercase hexadecimal digits.
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// TestNames is a FAIL_TO_PASS or PASS_TO_PASS list. It decodes from either
// encoding the SWE-bench datasets use: a JSON array of strings (the pinned
// revision) or a string holding a JSON-encoded array of strings (the
// princeton-nlp copies). See this file's "The schema" section.
type TestNames []string

// UnmarshalJSON decodes data as either encoding. JSON null, an empty
// string and an empty array all decode to an empty list.
//
// Anything else is an error, never an empty list. That is the point of
// writing this by hand: an empty FAIL_TO_PASS set resolves trivially in
// SWE-bench's own grading, so a decoder that quietly produced one from an
// encoding it did not understand would turn a decode bug into a perfect
// resolve rate.
func (t *TestNames) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*t = nil
		return nil
	}
	list := trimmed
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(trimmed, &encoded); err != nil {
			return fmt.Errorf("fixrate: a test-name list: %w", err)
		}
		if strings.TrimSpace(encoded) == "" {
			*t = nil
			return nil
		}
		list = []byte(encoded)
	}
	var names []string
	if err := json.Unmarshal(list, &names); err != nil {
		return fmt.Errorf("fixrate: a test-name list must be a JSON array of strings, or a string holding one: %w", err)
	}
	if len(names) == 0 {
		*t = nil
		return nil
	}
	*t = names
	return nil
}

// Instance is one SWE-bench record, with exactly the fields of the pinned
// revision's schema; see this file's "The schema" section for the source.
//
// It is a mirror rather than an import, for the same reason catalogue.go
// mirrors injected.json: the record format is someone else's documented
// contract, and a drift should be a loud decode error (DecodeInstances
// refuses unknown fields) rather than a silent misread.
type Instance struct {
	// InstanceID is "A formatted instance identifier, usually as
	// repo_owner__repo_name-PR-number" (dataset card).
	InstanceID string `json:"instance_id"`
	// Repo is the GitHub "owner/name" the instance comes from.
	Repo string `json:"repo"`
	// BaseCommit is the commit an attempt starts from: "the HEAD before
	// the solution PR is applied" (dataset card).
	BaseCommit string `json:"base_commit"`
	// EnvironmentSetupCommit is the "Commit hash to use for environment
	// setup and installation" (dataset card).
	EnvironmentSetupCommit string `json:"environment_setup_commit"`
	// ProblemStatement is "The issue title and body" (dataset card): the
	// only part of the record that becomes the goal.
	ProblemStatement string `json:"problem_statement"`
	// Patch is the gold patch -- the answer. It never reaches a goal.
	Patch string `json:"patch"`
	// TestPatch is the test-file patch from the solution PR, which carries
	// the FAIL_TO_PASS tests themselves. It never reaches a goal either.
	TestPatch string `json:"test_patch"`
	// HintsText is issue commentary from before the solution PR. The goal
	// deliberately omits it; see sweBenchGoal.
	HintsText string `json:"hints_text"`
	// CreatedAt is the solution PR's creation date, as the dataset writes
	// it. It is carried, not parsed.
	CreatedAt string `json:"created_at"`
	// Version is the "Installation version to use for running evaluation"
	// (dataset card).
	Version string `json:"version"`
	// Difficulty is the dataset's own time-to-fix label.
	Difficulty string `json:"difficulty"`
	// EvalScript, EvalType, Image and LogParser exist only in the
	// SWE-bench organisation's copy. EvalType selects the grading rule;
	// the other three are inputs to the official harness's containers,
	// which this file does not build.
	EvalScript string `json:"eval_script"`
	EvalType   string `json:"eval_type"`
	Image      string `json:"image"`
	LogParser  string `json:"log_parser"`
	// FailToPass are the tests the solution PR resolved: they must go from
	// failing to passing.
	FailToPass TestNames `json:"FAIL_TO_PASS"`
	// PassToPass are the tests that passed before and must still pass.
	PassToPass TestNames `json:"PASS_TO_PASS"`
}

// Validate reports whether i can become a measurable Task, naming every
// problem it finds rather than the first.
//
// It refuses an instance with no FAIL_TO_PASS tests even though upstream
// grades one as resolved, and one whose eval_type is not ported; this
// file's departures section says why each is the safe direction.
func (i Instance) Validate() error {
	var problems []string
	if strings.TrimSpace(i.InstanceID) == "" {
		problems = append(problems, "no instance_id")
	}
	if strings.TrimSpace(i.Repo) == "" {
		problems = append(problems, "no repo")
	}
	if strings.TrimSpace(i.BaseCommit) == "" {
		problems = append(problems, "no base_commit")
	}
	if strings.TrimSpace(i.ProblemStatement) == "" {
		problems = append(problems, "no problem_statement")
	}
	if len(i.FailToPass) == 0 {
		problems = append(problems, "no FAIL_TO_PASS tests: SWE-bench's own compute_fail_to_pass scores an empty set as 1, "+
			"so this instance would resolve without anything having been fixed")
	}
	switch i.EvalType {
	case "", sweBenchPassAndFail:
	default:
		problems = append(problems, fmt.Sprintf("eval_type %q is not ported: only %q grading is, "+
			"and it is the only eval_type the pinned revision contains", i.EvalType, sweBenchPassAndFail))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w %q: %s", ErrInvalidInstance, i.InstanceID, strings.Join(problems, "; "))
	}
	return nil
}

// Task is one unit of work both measurement arms are handed: the goal
// they are given verbatim, and the test names a status map is judged
// against.
//
// It is plain data on purpose. Everything needed to set up the workspace
// a task runs in -- repo, base commit, environment commit, version -- is
// carried, and nothing here acts on it.
type Task struct {
	// ID is the instance_id.
	ID string `json:"id"`
	// Corpus is the Source the caller declared, as "dataset@revision".
	Corpus string `json:"corpus"`
	// Goal is the text both arms receive, identically. See sweBenchGoal.
	Goal string `json:"goal"`
	// Provenance says what a number produced from this task can and
	// cannot claim. It is copied from this file's closing section, short
	// enough to travel with the task.
	Provenance string `json:"provenance"`

	FailToPass []string `json:"fail_to_pass"`
	PassToPass []string `json:"pass_to_pass"`

	Repo       string `json:"repo"`
	BaseCommit string `json:"base_commit"`
	EnvCommit  string `json:"environment_setup_commit"`
	Version    string `json:"version,omitempty"`
	Difficulty string `json:"difficulty,omitempty"`
	// EvalType is the grading rule Resolved applies. Instance.Task always
	// writes it explicitly, so a task never relies on a default.
	EvalType string `json:"eval_type"`
}

// Task converts i into a Task labelled with src, refusing an invalid
// source or instance.
func (i Instance) Task(src Source) (Task, error) {
	if err := src.Validate(); err != nil {
		return Task{}, err
	}
	if err := i.Validate(); err != nil {
		return Task{}, err
	}
	evalType := i.EvalType
	if evalType == "" {
		evalType = sweBenchPassAndFail
	}
	return Task{
		ID:         i.InstanceID,
		Corpus:     src.String(),
		Goal:       sweBenchGoal(i.ProblemStatement),
		Provenance: sweBenchProvenance(src),
		FailToPass: slices.Clone(i.FailToPass),
		PassToPass: slices.Clone(i.PassToPass),
		Repo:       i.Repo,
		BaseCommit: i.BaseCommit,
		EnvCommit:  i.EnvironmentSetupCommit,
		Version:    i.Version,
		Difficulty: i.Difficulty,
		EvalType:   evalType,
	}, nil
}

// sweBenchGoalTemplate is the shared goal both arms receive for a SWE-bench
// task.
//
// It asks for a regression test, as this roadmap settled, and asks it of
// both arms identically -- not because the template is applied twice, but
// because both arms read the one Goal string Task carries.
//
// What it leaves out is as deliberate as what it puts in. The gold patch
// is the answer and the test patch names the very tests the attempt will
// be judged on, so neither may appear. hints_text is left out too, so the
// goal is built from the issue alone; SWE-bench's reference prompt
// builders do not read it either
// (https://raw.githubusercontent.com/SWE-bench/SWE-bench/main/swebench/inference/make_datasets/create_instance.py),
// but whether any particular published submission used hints is UNKNOWN,
// which is one reason a number from this adapter is not a leaderboard
// figure.
//
// The problem statement is third-party text from a public issue tracker,
// inserted verbatim and fenced, and it reaches an agent with write access.
// That is inherent to the benchmark. Neither arm's tool set includes a
// shell (see controlTools), and containment of the workspace itself is the
// container work in 6c.
const sweBenchGoalTemplate = `Resolve the issue below in this repository.

<issue>
%s
</issue>

Fix the cause in the repository's source, and add a regression test that
fails without your change and passes with it.`

// sweBenchGoal renders the goal for a problem statement, verbatim.
func sweBenchGoal(problemStatement string) string {
	return fmt.Sprintf(sweBenchGoalTemplate, problemStatement)
}

// sweBenchProvenance is the disclosure every Task carries.
func sweBenchProvenance(src Source) string {
	return "corpus: " + src.String() + " (as declared by the caller; not verified offline) — converted by " +
		"test/fixrate's offline SWE-bench adapter. This task has not been run: converting it built no " +
		"container, checked out no repository and applied no patch. Resolved means what " + sweBenchGradingRef +
		" pass_and_fail grading means — every FAIL_TO_PASS test passes and no PASS_TO_PASS test regresses — " +
		"and cannot see a fix that is wrong in a way those tests do not exercise. A resolve rate over these " +
		"tasks is comparable to a published SWE-bench figure only if the statuses came from the official " +
		"harness's own containers, and even then the goal differs from any given submission's prompt."
}

// caseGrade is one expected test's verdict under SWE-bench's grading.
type caseGrade int

const (
	gradeSuccess caseGrade = iota
	gradeFailure
	// gradeUnknownStatus is departure (2): a status outside the
	// TestStatus enum, which upstream would drop from the denominator.
	gradeUnknownStatus
)

// Resolved applies SWE-bench's pass_and_fail grading to statuses, a map
// from test name to TestStatus value, and reports whether the task is
// fully resolved and why.
//
// See this file's "How an instance is judged" section for the rule and
// "Where this port departs from upstream" for the three places it
// deliberately does not follow it. Every verdict carries a reason, because
// a bare false is exactly the kind of number this harness refuses to
// produce.
func (t Task) Resolved(statuses map[string]string) (bool, string) {
	switch t.EvalType {
	case "", sweBenchPassAndFail:
	default:
		return false, fmt.Sprintf("eval_type %q is not ported (only %q grading is), so no verdict can be given",
			t.EvalType, sweBenchPassAndFail)
	}
	if len(t.FailToPass) == 0 {
		return false, "the task has no FAIL_TO_PASS tests, so nothing demonstrates a fix; SWE-bench's own " +
			"compute_fail_to_pass would score this 1, and this port refuses that free pass"
	}

	var f2pFailing, p2pRegressed, unknown []string
	for _, name := range t.FailToPass {
		switch gradeFailToPass(name, statuses) {
		case gradeFailure:
			f2pFailing = append(f2pFailing, name)
		case gradeUnknownStatus:
			unknown = append(unknown, name)
		case gradeSuccess:
		}
	}
	for _, name := range t.PassToPass {
		switch gradePassToPass(name, statuses) {
		case gradeFailure:
			p2pRegressed = append(p2pRegressed, name)
		case gradeUnknownStatus:
			unknown = append(unknown, name)
		case gradeSuccess:
		}
	}

	switch {
	case len(unknown) > 0:
		return false, fmt.Sprintf("%d expected test(s) carry a status outside SWE-bench's TestStatus enum (%s); "+
			"upstream would drop them from the denominator, and this port refuses to vouch for a verdict reached that way",
			len(unknown), strings.Join(unknown, ", "))
	case len(f2pFailing) == 0 && len(p2pRegressed) == 0:
		return true, fmt.Sprintf("all %d FAIL_TO_PASS test(s) pass and none of %d PASS_TO_PASS test(s) regressed",
			len(t.FailToPass), len(t.PassToPass))
	default:
		return false, fmt.Sprintf("FAIL_TO_PASS not passing: %d of %d [%s]; PASS_TO_PASS regressed: %d of %d [%s]",
			len(f2pFailing), len(t.FailToPass), strings.Join(f2pFailing, ", "),
			len(p2pRegressed), len(t.PassToPass), strings.Join(p2pRegressed, ", "))
	}
}

// gradeFailToPass ports check_pass_and_fail for one FAIL_TO_PASS name:
// test_passed succeeds, and anything else -- no resolvable entry, or
// FAILED, ERROR or SKIPPED -- fails (test_failed).
func gradeFailToPass(name string, statuses map[string]string) caseGrade {
	c := sweBenchCandidates(name, statuses)
	if slices.ContainsFunc(c, isUnknownStatus) {
		return gradeUnknownStatus
	}
	// Every candidate agrees on passing-ness (sweBenchCandidates
	// guarantees it), so the first speaks for all of them.
	if len(c) > 0 && isPassing(c[0]) {
		return gradeSuccess
	}
	return gradeFailure
}

// gradePassToPass ports check_maintained for one PASS_TO_PASS name:
// test_passed succeeds, a resolved SKIPPED also succeeds
// (test_maintained), and anything else fails.
func gradePassToPass(name string, statuses map[string]string) caseGrade {
	c := sweBenchCandidates(name, statuses)
	if slices.ContainsFunc(c, isUnknownStatus) {
		return gradeUnknownStatus
	}
	switch {
	case len(c) == 0:
		return gradeFailure
	case isPassing(c[0]):
		return gradeSuccess
	case !slices.ContainsFunc(c, func(s string) bool { return s != SWEBenchSkipped }):
		// Every candidate is SKIPPED. With several non-passing candidates
		// upstream would take the first in log order, which a map does not
		// carry; all-SKIPPED is the one case in which every order gives
		// the same answer. Departure (3).
		return gradeSuccess
	default:
		return gradeFailure
	}
}

// sweBenchCandidates ports _resolve_case, returning the statuses of every
// status-map entry name could resolve to.
//
// An exact key returns its one status. Failing that, a name with more '['
// than ']' -- a parametrized pytest id truncated mid-parameter -- returns
// the statuses of every key it is a prefix of, but only when they all
// agree on pass-versus-fail; any other name, or prefix matches that
// disagree, return nothing, which grades as an absent test. Upstream
// returns one key, the first match in log order; this returns them all,
// so its callers can tell when the choice of match could have mattered.
func sweBenchCandidates(name string, statuses map[string]string) []string {
	if s, ok := statuses[name]; ok {
		return []string{s}
	}
	if strings.Count(name, "[") <= strings.Count(name, "]") {
		return nil
	}
	var matched []string
	for key, s := range statuses {
		if strings.HasPrefix(key, name) {
			matched = append(matched, s)
		}
	}
	for _, s := range matched {
		if isPassing(s) != isPassing(matched[0]) {
			return nil
		}
	}
	return matched
}

// isPassing reports whether status is one test_passed accepts.
func isPassing(status string) bool { return status == SWEBenchPassed || status == SWEBenchXFail }

// isUnknownStatus reports whether status is outside SWE-bench's TestStatus
// enum.
func isUnknownStatus(status string) bool {
	switch status {
	case SWEBenchPassed, SWEBenchFailed, SWEBenchSkipped, SWEBenchError, SWEBenchXFail:
		return false
	default:
		return true
	}
}

// DecodeInstances reads SWE-bench records from r, either as one JSON array
// or as a JSONL stream of one object per line, and returns them unvalidated
// -- whether a record can become a Task is Validate's question.
//
// It refuses rather than skips: a record carrying a field the pinned schema
// does not have (the schema is fixed, so drift means the input is not what
// the pin says it is), a value it cannot decode, or anything after a
// top-level array. Silently skipping any of these would drop records from
// a corpus without a word, and a resolve rate over a quietly shrunken
// corpus is not the number it claims to be.
func DecodeInstances(r io.Reader) ([]Instance, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("fixrate: reading SWE-bench instances: %w", err)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()

	if trimmed[0] == '[' {
		var out []Instance
		if err := dec.Decode(&out); err != nil {
			return nil, fmt.Errorf("fixrate: decoding a JSON array of SWE-bench instances: %w", err)
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, errors.New("fixrate: decoding a JSON array of SWE-bench instances: unexpected data after the array")
		}
		return out, nil
	}

	var out []Instance
	for n := 1; ; n++ {
		var inst Instance
		err := dec.Decode(&inst)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("fixrate: decoding SWE-bench instance %d of a JSONL stream: %w", n, err)
		}
		out = append(out, inst)
	}
}
