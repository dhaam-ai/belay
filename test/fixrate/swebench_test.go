//go:build fixrate

package fixrate

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func swebenchFixture(t *testing.T, name string) []Instance {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "swebench", name)) //nolint:gosec // name is always a literal from this file, under this package's own testdata
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	got, err := DecodeInstances(f)
	if err != nil {
		t.Fatalf("DecodeInstances(%s) error = %v", name, err)
	}
	return got
}

// swebenchInstance returns the i'th record of a fixture file, failing the
// test with a message rather than panicking when the file decoded to fewer
// records than the test needs.
func swebenchInstance(t *testing.T, name string, i int) Instance {
	t.Helper()
	all := swebenchFixture(t, name)
	if i >= len(all) {
		t.Fatalf("DecodeInstances(%s) returned %d instance(s), want at least %d", name, len(all), i+1)
	}
	return all[i]
}

// TestInstance_FieldNamesMatchThePinnedSchema pins Instance's JSON field
// names to the schema of the pinned revision, exactly. The 17 names below
// are copied from the Hugging Face datasets-server's feature list for
// SWE-bench/SWE-bench_Verified at 78f471bf655a3137b2e8a75af1501690ec009ec3
// (https://datasets-server.huggingface.co/info?dataset=SWE-bench%2FSWE-bench_Verified),
// not recalled: a misspelt tag ("fail_to_pass" for "FAIL_TO_PASS") would
// decode every record to an empty list without an error, and an empty
// FAIL_TO_PASS resolves trivially in SWE-bench's own grading.
func TestInstance_FieldNamesMatchThePinnedSchema(t *testing.T) {
	t.Parallel()

	want := []string{
		"FAIL_TO_PASS", "PASS_TO_PASS", "base_commit", "created_at", "difficulty",
		"environment_setup_commit", "eval_script", "eval_type", "hints_text", "image",
		"instance_id", "log_parser", "patch", "problem_statement", "repo", "test_patch", "version",
	}
	var got []string
	typ := reflect.TypeFor[Instance]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		got = append(got, name)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("Instance's JSON field names = %v\nwant exactly the pinned schema's %v", got, want)
	}
}

// TestTestNames_BothEncodings is the reason this type exists. The pinned
// revision encodes FAIL_TO_PASS as a JSON array of strings; the older
// princeton-nlp copies, and the dataset card's own prose, encode it as a
// JSON-encoded STRING. Both are real and an adapter must read either.
func TestTestNames_BothEncodings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"native array", `["a::t1","a::t2"]`, []string{"a::t1", "a::t2"}, false},
		{"json-encoded string", `"[\"a::t1\", \"a::t2\"]"`, []string{"a::t1", "a::t2"}, false},
		{"empty array", `[]`, nil, false},
		{"empty json-encoded array", `"[]"`, nil, false},
		{"json null", `null`, nil, false},
		{"empty string", `""`, nil, false},
		// An encoding that is neither must be an error, never a silent
		// empty list: an empty FAIL_TO_PASS resolves trivially in
		// SWE-bench's own grading, so quietly producing one would turn a
		// decode bug into a 100% resolve rate.
		{"a bare number", `12`, nil, true},
		{"a string that is not JSON", `"tests/test_core.py::test_x"`, nil, true},
		{"an array of numbers", `[1,2]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got TestNames
			err := got.UnmarshalJSON([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalJSON(%s) = %v, nil; want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON(%s) error = %v", tt.in, err)
			}
			if !slices.Equal([]string(got), tt.want) {
				t.Errorf("UnmarshalJSON(%s) = %v, want %v", tt.in, []string(got), tt.want)
			}
		})
	}
}

func TestDecodeInstances_ArrayAndJSONL(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"instances-array.json", "instances-jsonl.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := swebenchFixture(t, name)
			if len(got) != 2 {
				t.Fatalf("DecodeInstances() returned %d instance(s), want 2", len(got))
			}
			first := got[0]
			switch {
			case first.InstanceID != "example__widget-1234":
				t.Errorf("InstanceID = %q, want %q", first.InstanceID, "example__widget-1234")
			case first.Repo != "example/widget":
				t.Errorf("Repo = %q, want %q", first.Repo, "example/widget")
			case first.BaseCommit != "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c":
				t.Errorf("BaseCommit = %q, unexpected", first.BaseCommit)
			case first.Version != "2.1":
				t.Errorf("Version = %q, want %q", first.Version, "2.1")
			}
			// Both encodings must land on the same decoded names.
			if want := []string{"tests/test_core.py::test_resize_rounds_half_up"}; !slices.Equal(first.FailToPass, want) {
				t.Errorf("FailToPass = %v, want %v", first.FailToPass, want)
			}
			if want := []string{
				"tests/test_core.py::test_resize_zero",
				"tests/test_core.py::test_resize_identity",
			}; !slices.Equal(first.PassToPass, want) {
				t.Errorf("PassToPass = %v, want %v", first.PassToPass, want)
			}
		})
	}
}

func TestDecodeInstances_Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{"not json at all", "this is not json"},
		{"a truncated array", `[{"instance_id":"x"}`},
		{"an array of non-records", `[1, 2]`},
		// One document per input: an array followed by anything else is
		// not an array of instances, and silently ignoring the rest would
		// drop records without a word.
		{"an array followed by more data", `[{"instance_id":"x"}] [{"instance_id":"y"}]`},
		{"a jsonl line that is not an object", "12\n"},
		// A field this adapter does not know is a record from a schema it
		// was not written against. That must be loud: the pinned schema
		// is fixed, so drift means the input is not what the pin says.
		{"a record with an unknown field", `{"instance_id":"x","FAIL_TO_FAIL":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got, err := DecodeInstances(strings.NewReader(tt.in)); err == nil {
				t.Errorf("DecodeInstances() = %d instance(s), nil error; want an error", len(got))
			}
		})
	}
}

// TestDecodeInstances_OneObjectIsOneRecord pins a case the refusals above
// must not swallow: a single JSON object is a valid one-record JSONL
// stream. Whether that record is usable is Validate's question, not the
// decoder's.
func TestDecodeInstances_OneObjectIsOneRecord(t *testing.T) {
	t.Parallel()

	got, err := DecodeInstances(strings.NewReader(`{"instance_id":"x"}`))
	if err != nil {
		t.Fatalf("DecodeInstances() error = %v", err)
	}
	if len(got) != 1 || got[0].InstanceID != "x" {
		t.Fatalf("DecodeInstances() = %+v, want one record with instance_id x", got)
	}
	if err := got[0].Validate(); !errors.Is(err, ErrInvalidInstance) {
		t.Errorf("Validate() error = %v, want the record refused as incomplete", err)
	}
}

func TestSource_Validate(t *testing.T) {
	t.Parallel()

	if err := PinnedSource.Validate(); err != nil {
		t.Fatalf("PinnedSource.Validate() error = %v", err)
	}
	if PinnedSource.Dataset != SWEBenchDataset || PinnedSource.Revision != SWEBenchRevision {
		t.Fatalf("PinnedSource = %+v, want %s@%s", PinnedSource, SWEBenchDataset, SWEBenchRevision)
	}

	tests := []struct {
		name string
		src  Source
	}{
		{"no dataset", Source{Revision: SWEBenchRevision}},
		{"no revision", Source{Dataset: SWEBenchDataset}},
		// A branch name is a moving target, and a number labelled with one
		// cannot be traced back to the records that produced it.
		{"a branch, not a commit", Source{Dataset: SWEBenchDataset, Revision: "main"}},
		{"an abbreviated sha", Source{Dataset: SWEBenchDataset, Revision: "78f471bf"}},
		{"not hex", Source{Dataset: SWEBenchDataset, Revision: strings.Repeat("z", 40)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.src.Validate(); !errors.Is(err, ErrInvalidSource) {
				t.Errorf("Validate() error = %v, want it to wrap ErrInvalidSource", err)
			}
		})
	}
}

func TestInstance_Validate(t *testing.T) {
	t.Parallel()

	valid := swebenchInstance(t, "instances-array.json", 0)
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() on a well-formed instance error = %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Instance)
		wantErr bool
	}{
		{"no instance id", func(i *Instance) { i.InstanceID = "" }, true},
		{"no repo", func(i *Instance) { i.Repo = "" }, true},
		{"no base commit", func(i *Instance) { i.BaseCommit = "" }, true},
		{"no problem statement", func(i *Instance) { i.ProblemStatement = "   " }, true},
		// The trap: SWE-bench's own compute_fail_to_pass returns 1 for an
		// empty set, so an instance with no FAIL_TO_PASS resolves
		// trivially in both arms and measures nothing.
		{"no fail-to-pass tests", func(i *Instance) { i.FailToPass = nil }, true},
		// Only pass_and_fail grading is ported, and it is the only
		// eval_type the pinned revision contains. A record without the
		// column (the older princeton-nlp copies) gets
		// get_eval_tests_report's own default, which is pass_and_fail.
		{"eval_type pass_and_fail", func(i *Instance) { i.EvalType = "pass_and_fail" }, false},
		{"eval_type absent", func(i *Instance) { i.EvalType = "" }, false},
		{"eval_type fail_only is not ported", func(i *Instance) { i.EvalType = "fail_only" }, true},
		{"eval_type unknown", func(i *Instance) { i.EvalType = "pytest" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inst := valid
			tt.mutate(&inst)
			err := inst.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidInstance) {
				t.Errorf("Validate() error = %v, want it to wrap ErrInvalidInstance", err)
			}
		})
	}
}

func TestInstance_Validate_RefusesTheTrivialInstanceFixture(t *testing.T) {
	t.Parallel()

	got := swebenchFixture(t, "instance-no-fail-to-pass.json")
	if len(got) != 1 {
		t.Fatalf("DecodeInstances() returned %d instance(s), want 1", len(got))
	}
	if err := got[0].Validate(); !errors.Is(err, ErrInvalidInstance) {
		t.Errorf("Validate() error = %v, want it to wrap ErrInvalidInstance", err)
	}
	if _, err := got[0].Task(PinnedSource); !errors.Is(err, ErrInvalidInstance) {
		t.Errorf("Task() error = %v, want it to wrap ErrInvalidInstance", err)
	}
}

func TestInstance_Task(t *testing.T) {
	t.Parallel()

	inst := swebenchInstance(t, "instances-array.json", 0)
	got, err := inst.Task(PinnedSource)
	if err != nil {
		t.Fatalf("Task() error = %v", err)
	}

	switch {
	case got.ID != inst.InstanceID:
		t.Errorf("ID = %q, want the instance_id %q", got.ID, inst.InstanceID)
	case got.Repo != inst.Repo:
		t.Errorf("Repo = %q, want %q", got.Repo, inst.Repo)
	case got.BaseCommit != inst.BaseCommit:
		t.Errorf("BaseCommit = %q, want %q", got.BaseCommit, inst.BaseCommit)
	case got.EnvCommit != inst.EnvironmentSetupCommit:
		t.Errorf("EnvCommit = %q, want %q", got.EnvCommit, inst.EnvironmentSetupCommit)
	case got.Difficulty != inst.Difficulty:
		t.Errorf("Difficulty = %q, want %q", got.Difficulty, inst.Difficulty)
	case got.EvalType != "pass_and_fail":
		t.Errorf("EvalType = %q, want %q", got.EvalType, "pass_and_fail")
	case !slices.Equal(got.FailToPass, inst.FailToPass):
		t.Errorf("FailToPass = %v, want %v", got.FailToPass, inst.FailToPass)
	case !slices.Equal(got.PassToPass, inst.PassToPass):
		t.Errorf("PassToPass = %v, want %v", got.PassToPass, inst.PassToPass)
	}

	// The corpus label carries the source the CALLER declared, so a task
	// lifted out of a report still says which revision of which dataset
	// it was claimed to come from.
	if want := SWEBenchDataset + "@" + SWEBenchRevision; got.Corpus != want {
		t.Errorf("Corpus = %q, want %q", got.Corpus, want)
	}

	// The goal must carry the problem statement verbatim and must ask for
	// a regression test — identically for both arms, which is guaranteed
	// by both arms reading this one string.
	if !strings.Contains(got.Goal, inst.ProblemStatement) {
		t.Errorf("Goal = %q, want it to carry the problem statement verbatim", got.Goal)
	}
	if !strings.Contains(strings.ToLower(got.Goal), "regression test") {
		t.Errorf("Goal = %q, want it to ask for a regression test", got.Goal)
	}
	// The gold patch must NEVER reach the goal: it is the answer.
	if strings.Contains(got.Goal, inst.Patch) || strings.Contains(got.Goal, "int(n * f + 0.5)") {
		t.Errorf("Goal leaks the gold patch:\n%s", got.Goal)
	}
	// Nor may the dataset's own test patch, which names the very test
	// FAIL_TO_PASS will be judged on.
	if strings.Contains(got.Goal, inst.TestPatch) || strings.Contains(got.Goal, "test_resize_rounds_half_up") {
		t.Errorf("Goal leaks the test patch or a FAIL_TO_PASS test name:\n%s", got.Goal)
	}
	if got.Provenance == "" {
		t.Error("Provenance is empty; a task must say what a number produced from it can and cannot claim")
	}
	for _, want := range []string{"container", "not been run", PinnedSource.String()} {
		if !strings.Contains(strings.ToLower(got.Provenance), strings.ToLower(want)) {
			t.Errorf("Provenance = %q, want it to mention %q", got.Provenance, want)
		}
	}
}

// TestInstance_Task_ExcludesHints pins a choice this adapter makes, not
// one it inherits: the goal is built from the issue alone. SWE-bench's
// reference prompt builders (swebench/inference/make_datasets/
// create_instance.py) do not read hints_text either, but nothing here
// claims every published submission made the same choice.
func TestInstance_Task_ExcludesHints(t *testing.T) {
	t.Parallel()

	inst := swebenchInstance(t, "instances-array.json", 1)
	if inst.HintsText == "" {
		t.Fatal("this fixture instance is supposed to carry hints_text")
	}
	got, err := inst.Task(PinnedSource)
	if err != nil {
		t.Fatalf("Task() error = %v", err)
	}
	if strings.Contains(got.Goal, inst.HintsText) {
		t.Errorf("Goal contains hints_text %q:\n%s", inst.HintsText, got.Goal)
	}
}

func TestInstance_Task_RefusesABadSource(t *testing.T) {
	t.Parallel()

	inst := swebenchInstance(t, "instances-array.json", 0)
	if _, err := inst.Task(Source{Dataset: SWEBenchDataset, Revision: "main"}); !errors.Is(err, ErrInvalidSource) {
		t.Errorf("Task() error = %v, want it to wrap ErrInvalidSource", err)
	}
}

// TestTask_Resolved ports SWE-bench's pass_and_fail grading, and every
// expectation is taken from grading.py at
// ca6e4e0d252f32f8762625b73575d5dee49d0a5a rather than invented:
//
//	FAIL_TO_PASS (check_pass_and_fail): PASSED or XFAIL succeeds;
//	  FAILED, ERROR, SKIPPED or no entry fails.
//	PASS_TO_PASS (check_maintained): as above, except that SKIPPED
//	  succeeds — "a skipped test is not a regression, unlike for F2P".
//	FULL resolution: every FAIL_TO_PASS and every PASS_TO_PASS succeeds.
func TestTask_Resolved(t *testing.T) {
	t.Parallel()

	task := Task{
		ID:         "example__widget-1234",
		FailToPass: []string{"f1", "f2"},
		PassToPass: []string{"p1"},
	}

	tests := []struct {
		name     string
		statuses map[string]string
		want     bool
	}{
		{
			name:     "everything passes",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchPassed, "p1": SWEBenchPassed},
			want:     true,
		},
		{
			name:     "xfail counts as passed",
			statuses: map[string]string{"f1": SWEBenchXFail, "f2": SWEBenchPassed, "p1": SWEBenchXFail},
			want:     true,
		},
		{
			name:     "one fail-to-pass still fails",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchFailed, "p1": SWEBenchPassed},
			want:     false,
		},
		{
			name:     "a fail-to-pass test went missing",
			statuses: map[string]string{"f1": SWEBenchPassed, "p1": SWEBenchPassed},
			want:     false,
		},
		{
			name:     "a skipped fail-to-pass test is a failure",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchSkipped, "p1": SWEBenchPassed},
			want:     false,
		},
		{
			name:     "a fail-to-pass error is a failure",
			statuses: map[string]string{"f1": SWEBenchError, "f2": SWEBenchPassed, "p1": SWEBenchPassed},
			want:     false,
		},
		{
			name:     "a skipped pass-to-pass test is not a regression",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchPassed, "p1": SWEBenchSkipped},
			want:     true,
		},
		{
			name:     "a pass-to-pass failure is a regression",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchPassed, "p1": SWEBenchFailed},
			want:     false,
		},
		{
			name:     "a pass-to-pass error is a regression",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchPassed, "p1": SWEBenchError},
			want:     false,
		},
		{
			name:     "a missing pass-to-pass test is a regression",
			statuses: map[string]string{"f1": SWEBenchPassed, "f2": SWEBenchPassed},
			want:     false,
		},
		{
			name:     "nothing ran at all",
			statuses: map[string]string{},
			want:     false,
		},
		// A departure, and a deliberate one: upstream drops a test whose
		// status is outside its TestStatus enum from both its success and
		// failure lists, which only ever shrinks the denominator — so a
		// FAIL_TO_PASS set made entirely of unknown statuses would score
		// 1. Statuses from SWE-bench's own log parsers never fall outside
		// the enum, so this only bites a status map from somewhere else,
		// and there it refuses to vouch.
		{
			name:     "an unknown status is not a pass",
			statuses: map[string]string{"f1": "WOBBLY", "f2": SWEBenchPassed, "p1": SWEBenchPassed},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := task.Resolved(tt.statuses)
			if got != tt.want {
				t.Errorf("Resolved(%v) = %v (%s), want %v", tt.statuses, got, reason, tt.want)
			}
			if reason == "" {
				t.Error("Resolved() returned an empty reason; every verdict must say why")
			}
		})
	}
}

// TestTask_Resolved_TruncatedParametrizedIDs ports _resolve_case, whose
// own docstring explains why exact-name lookup is not enough: "676
// expected ids in SWE-bench_Verified are truncated mid-parameter (issue
// #290)". An expected id with more '[' than ']' prefix-matches the status
// map, but only when every candidate agrees on pass-versus-fail.
func TestTask_Resolved_TruncatedParametrizedIDs(t *testing.T) {
	t.Parallel()

	const truncated = "tests/test_g.py::test_grammar[log(photon"

	tests := []struct {
		name       string
		failToPass []string
		passToPass []string
		statuses   map[string]string
		want       bool
	}{
		{
			name:       "a truncated id resolves to its one passing match",
			failToPass: []string{truncated},
			statuses:   map[string]string{truncated + ")]": SWEBenchPassed},
			want:       true,
		},
		{
			name:       "matches that grade alike resolve (PASSED and XFAIL)",
			failToPass: []string{truncated},
			statuses:   map[string]string{truncated + ")]": SWEBenchPassed, truncated + "s)]": SWEBenchXFail},
			want:       true,
		},
		{
			name:       "matches that disagree resolve to nothing, which is a failure",
			failToPass: []string{truncated},
			statuses:   map[string]string{truncated + ")]": SWEBenchPassed, truncated + "s)]": SWEBenchFailed},
			want:       false,
		},
		{
			name:       "a balanced id never prefix-matches",
			failToPass: []string{"tests/test_g.py::test_h"},
			statuses:   map[string]string{"tests/test_g.py::test_h[1]": SWEBenchPassed},
			want:       false,
		},
		{
			name:       "a truncated pass-to-pass id whose matches are all skipped is maintained",
			failToPass: []string{"f1"},
			passToPass: []string{truncated},
			statuses: map[string]string{
				"f1": SWEBenchPassed, truncated + ")]": SWEBenchSkipped, truncated + "s)]": SWEBenchSkipped,
			},
			want: true,
		},
		// The one corner where upstream's verdict depends on information a
		// status map does not carry: SKIPPED and FAILED agree on "not
		// passing", so _resolve_case returns whichever came first in log
		// order, and test_maintained then says yes for SKIPPED and no for
		// FAILED. A Go map has no log order, so this port takes the
		// conservative verdict.
		{
			name:       "mixed skipped and failed matches are not maintained",
			failToPass: []string{"f1"},
			passToPass: []string{truncated},
			statuses: map[string]string{
				"f1": SWEBenchPassed, truncated + ")]": SWEBenchSkipped, truncated + "s)]": SWEBenchFailed,
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := Task{ID: "t", FailToPass: tt.failToPass, PassToPass: tt.passToPass}
			got, reason := task.Resolved(tt.statuses)
			if got != tt.want {
				t.Errorf("Resolved(%v) = %v (%s), want %v", tt.statuses, got, reason, tt.want)
			}
		})
	}
}

// TestTask_Resolved_EmptyFailToPassIsNeverTriviallyResolved is the guard
// on the other place this adapter deliberately DEPARTS from SWE-bench's
// grading. Its compute_fail_to_pass returns 1 for an empty set, so an
// instance with no FAIL_TO_PASS resolves for free. Validate refuses such an
// instance, and Resolved refuses it too, so the free pass cannot be
// reached by either route.
func TestTask_Resolved_EmptyFailToPassIsNeverTriviallyResolved(t *testing.T) {
	t.Parallel()

	task := Task{ID: "t", PassToPass: []string{"p1"}}
	got, reason := task.Resolved(map[string]string{"p1": SWEBenchPassed})
	if got {
		t.Errorf("Resolved() = true (%s); a task with no FAIL_TO_PASS tests proves nothing and must not resolve", reason)
	}
	if !strings.Contains(reason, "FAIL_TO_PASS") {
		t.Errorf("reason = %q, want it to name the empty FAIL_TO_PASS set", reason)
	}
}

// TestTask_Resolved_RefusesAnUnportedEvalType: a Task built by hand rather
// than through Instance.Task could carry fail_only, whose grading is
// different (only an explicit FAILED fails, by exact key) and is not
// ported. Grading it by the pass_and_fail rule would be a wrong verdict
// with no error, so Resolved refuses.
func TestTask_Resolved_RefusesAnUnportedEvalType(t *testing.T) {
	t.Parallel()

	task := Task{ID: "t", EvalType: "fail_only", FailToPass: []string{"f1"}}
	got, reason := task.Resolved(map[string]string{"f1": SWEBenchPassed})
	if got {
		t.Errorf("Resolved() = true (%s), want false for an eval_type this port does not implement", reason)
	}
	if !strings.Contains(reason, "fail_only") {
		t.Errorf("reason = %q, want it to name the unsupported eval_type", reason)
	}
}

// TestSWEBenchFixturesAreSynthetic is a licence guard, not a style check.
// The SWE-bench dataset declares no licence (see swebench.go), so no
// dataset record may be committed here. Real instance IDs are
// "<owner>__<repo>-<PR number>" over well-known open-source projects, so
// this refuses any record file naming one of those projects.
//
// It passes against the stubbed adapter because it reads the files, not
// the adapter. It was checked by mutation instead: planting
// "django/django" in instances-array.json made it fail with the expected
// message, and reverting the file made it pass again.
func TestSWEBenchFixturesAreSynthetic(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("testdata", "swebench")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	// The repositories SWE-bench and SWE-bench_Verified actually draw
	// from; none may appear in a committed record file.
	realRepos := []string{
		"astropy", "django", "flask", "matplotlib", "pallets", "psf/requests",
		"pydata", "pylint", "pytest-dev", "scikit-learn", "seaborn", "sphinx-doc",
		"sqlfluff", "sympy", "xarray",
	}
	// Only record files are scanned. The directory's README is prose that
	// must be free to name the real dataset it explains it is not using.
	var scanned int
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".json" && filepath.Ext(e.Name()) != ".jsonl") {
			continue
		}
		scanned++
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a fixed testdata directory in this package
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		lower := strings.ToLower(string(data))
		for _, repo := range realRepos {
			if strings.Contains(lower, repo) {
				t.Errorf("%s mentions %q; only synthetic fixtures may be committed until the dataset licence is established",
					e.Name(), repo)
			}
		}
	}
	// A guard that scanned nothing guards nothing: if the fixtures move or
	// are renamed, this must fail rather than pass vacuously.
	if scanned == 0 {
		t.Fatalf("no .json or .jsonl record files found under %s; this guard scanned nothing", dir)
	}
}
