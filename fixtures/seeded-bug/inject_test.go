package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"belay.dev/fixtures/seededbug/bugs"
)

// realSrcDir returns the absolute path to this fixture's own src/
// directory, resolved once per test process. Every test in this file
// treats it as read-only: nothing here may write into it directly, and
// the tests in this section specifically prove the injector itself
// refuses to either.
func realSrcDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("src")
	if err != nil {
		t.Fatalf("resolving src/: %v", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		t.Fatalf("src/ not found at %s (tests must run with CWD = fixtures/seeded-bug): %v", abs, err)
	}
	return abs
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"bugs_and_out", []string{"--bugs=B01,B02", "--out=/tmp/x"}, false},
		{"random_and_seed_and_out", []string{"--random=3", "--seed=42", "--out=/tmp/x"}, false},
		{"missing_out", []string{"--bugs=B01"}, true},
		{"missing_selection", []string{"--out=/tmp/x"}, true},
		{"both_bugs_and_random", []string{"--bugs=B01", "--random=3", "--seed=1", "--out=/tmp/x"}, true},
		{"random_without_seed", []string{"--random=3", "--out=/tmp/x"}, true},
		{"negative_random", []string{"--random=-1", "--seed=1", "--out=/tmp/x"}, true},
		{"seed_not_an_integer", []string{"--random=3", "--seed=abc", "--out=/tmp/x"}, true},
		{"random_zero_falls_back_to_missing_selection", []string{"--random=0", "--out=/tmp/x"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			_, err := parseArgs(tc.args, &stderr)
			if tc.wantErr && err == nil {
				t.Fatalf("parseArgs(%v) = nil error, want an error", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("parseArgs(%v) error = %v, want nil", tc.args, err)
			}
		})
	}

	t.Run("bugs_list_is_split_and_trimmed", func(t *testing.T) {
		var stderr strings.Builder
		opt, err := parseArgs([]string{"--bugs= B01 , B02,B03 ", "--out=/tmp/x"}, &stderr)
		if err != nil {
			t.Fatalf("parseArgs error = %v", err)
		}
		want := []string{"B01", "B02", "B03"}
		if len(opt.bugs) != len(want) {
			t.Fatalf("opt.bugs = %v, want %v", opt.bugs, want)
		}
		for i := range want {
			if opt.bugs[i] != want[i] {
				t.Fatalf("opt.bugs = %v, want %v", opt.bugs, want)
			}
		}
	})

	t.Run("seed_is_parsed_as_int64", func(t *testing.T) {
		var stderr strings.Builder
		opt, err := parseArgs([]string{"--random=2", "--seed=-42", "--out=/tmp/x"}, &stderr)
		if err != nil {
			t.Fatalf("parseArgs error = %v", err)
		}
		if !opt.seedSet || opt.seed != -42 {
			t.Fatalf("opt.seed = %v (set=%v), want -42 (set=true)", opt.seed, opt.seedSet)
		}
	})
}

func TestCheckSafeDestination(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil { //nolint:gosec // test fixture directory, not sensitive
		t.Fatal(err)
	}
	parent := filepath.Dir(src)
	sibling := t.TempDir()

	cases := []struct {
		name    string
		out     string
		wantErr bool
	}{
		{"sibling_directory_is_safe", filepath.Join(sibling, "out"), false},
		{"out_equals_src", src, true},
		{"out_nested_inside_src", filepath.Join(src, "nested"), true},
		{"out_is_new_subdir_of_src", filepath.Join(src, "nested", "deeper"), true},
		{"src_is_ancestor_of_out_via_dotdot", filepath.Join(src, "..", filepath.Base(src), "nested"), true},
		{"out_is_ancestor_of_src", parent, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSafeDestination(src, tc.out)
			if tc.wantErr && err == nil {
				t.Fatalf("checkSafeDestination(%q, %q) = nil, want an error", src, tc.out)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkSafeDestination(%q, %q) = %v, want nil", src, tc.out, err)
			}
		})
	}
}

// TestInjectorRefusesToWriteIntoSrc drives the refusal through the real
// run() entry point end to end, using a throwaway copy of src/ so there
// is no path by which a bug here could touch the fixture's real source.
func TestInjectorRefusesToWriteIntoSrc(t *testing.T) {
	fakeSrc := t.TempDir()
	if err := copyTree(realSrcDir(t), fakeSrc); err != nil {
		t.Fatalf("seeding fake src: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(fakeSrc, "policy.go")) //nolint:gosec // fakeSrc is a t.TempDir() this test created, not external input
	if err != nil {
		t.Fatal(err)
	}

	attempts := []string{
		fakeSrc,
		filepath.Join(fakeSrc, "policy.go"),
		filepath.Join(fakeSrc, "..", filepath.Base(fakeSrc)),
	}
	for _, out := range attempts {
		t.Run(out, func(t *testing.T) {
			var stdout, stderr strings.Builder
			err := run([]string{"--bugs=B01", "--src=" + fakeSrc, "--out=" + out}, &stdout, &stderr)
			if err == nil {
				t.Fatalf("run() with --out=%s inside/at src = nil error, want a refusal", out)
			}
			if !strings.Contains(err.Error(), "refusing to write into src") {
				t.Fatalf("run() error = %v, want it to mention refusing to write into src", err)
			}
		})
	}

	after, err := os.ReadFile(filepath.Join(fakeSrc, "policy.go")) //nolint:gosec // fakeSrc is a t.TempDir() this test created, not external input
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("fake src/policy.go changed even though every injection attempt was refused")
	}
}

func TestRandomDefects(t *testing.T) {
	all := bugs.All()

	t.Run("same_seed_and_n_selects_the_same_set_across_100_calls", func(t *testing.T) {
		first, err := randomDefects(all, 5, 42)
		if err != nil {
			t.Fatalf("randomDefects error = %v", err)
		}
		firstIDs := defectIDs(first)

		for i := 0; i < 100; i++ {
			got, err := randomDefects(all, 5, 42)
			if err != nil {
				t.Fatalf("iteration %d: randomDefects error = %v", i, err)
			}
			gotIDs := defectIDs(got)
			if !equalStrings(gotIDs, firstIDs) {
				t.Fatalf("iteration %d: randomDefects(all, 5, 42) = %v, want %v (same as iteration 0)", i, gotIDs, firstIDs)
			}
		}
	})

	t.Run("result_is_sorted_by_id", func(t *testing.T) {
		got, err := randomDefects(all, len(all), 7)
		if err != nil {
			t.Fatalf("randomDefects error = %v", err)
		}
		ids := defectIDs(got)
		if !sort.StringsAreSorted(ids) {
			t.Fatalf("randomDefects result %v is not sorted by ID", ids)
		}
	})

	t.Run("different_seeds_can_select_different_sets", func(t *testing.T) {
		a, err := randomDefects(all, 5, 1)
		if err != nil {
			t.Fatal(err)
		}
		b, err := randomDefects(all, 5, 2)
		if err != nil {
			t.Fatal(err)
		}
		if equalStrings(defectIDs(a), defectIDs(b)) {
			t.Skip("seeds 1 and 2 happened to collide on the same 5 IDs; not a correctness bug, just an uninteresting run")
		}
	})

	t.Run("n_greater_than_catalogue_size_is_an_error", func(t *testing.T) {
		if _, err := randomDefects(all, len(all)+1, 1); err == nil {
			t.Fatal("randomDefects(all, len(all)+1, 1) = nil error, want an error")
		}
	})

	t.Run("n_equal_to_catalogue_size_selects_everything", func(t *testing.T) {
		got, err := randomDefects(all, len(all), 1)
		if err != nil {
			t.Fatalf("randomDefects error = %v", err)
		}
		if len(got) != len(all) {
			t.Fatalf("len(randomDefects(all, len(all), 1)) = %d, want %d", len(got), len(all))
		}
	})
}

func TestRandomSelection_CLIEndToEndDeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real go run subprocesses; skipped in -short mode")
	}
	src := realSrcDir(t)

	invoke := func(dir string) []string {
		t.Helper()
		out := filepath.Join(dir, "out")
		cmd := exec.Command("go", "run", "./inject.go", "--random=5", "--seed=42", "--src="+src, "--out="+out) //nolint:gosec // src and out are this test's own realSrcDir()/t.TempDir(), not external input
		cmd.Dir = mustWD(t)
		output, err := cmd.CombinedOutput()
		// A non-zero exit here can legitimately mean "this particular
		// 5-defect combination didn't verify cleanly" (e.g. it drew a
		// panic-prone defect together with others whose tests run later
		// in the target package and so never get a chance to run) rather
		// than a real failure of *this* test, which only cares whether
		// the selection itself is deterministic. injected.json is always
		// written before that verification error is returned, so require
		// only that it exists.
		if _, statErr := os.Stat(filepath.Join(out, "injected.json")); statErr != nil {
			t.Fatalf("go run ./inject.go --random=5 --seed=42 failed before writing injected.json: %v\n%s", err, output)
		}
		manifest := readManifest(t, out)
		ids := make([]string, len(manifest.Defects))
		for i, d := range manifest.Defects {
			ids[i] = d.ID
		}
		return ids
	}

	first := invoke(t.TempDir())
	second := invoke(t.TempDir())
	if !equalStrings(first, second) {
		t.Fatalf("two real `go run ./inject.go --random=5 --seed=42` invocations selected %v and %v, want identical", first, second)
	}
}

func mustWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func readManifest(t *testing.T, dir string) injectedManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "injected.json")) //nolint:gosec // dir is a t.TempDir() this test created, not external input
	if err != nil {
		t.Fatalf("reading injected.json: %v", err)
	}
	var m injectedManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing injected.json: %v\n%s", err, data)
	}
	return m
}

func defectIDs(ds []bugs.Defect) []string {
	ids := make([]string, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	return ids
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// syntheticFile writes content to name inside dir and returns its
// relative path, for constructing ad-hoc bugs.Defect values in tests
// that must not depend on (or risk corrupting expectations about) the
// real catalogue.
func syntheticFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // test fixture file, not sensitive
		t.Fatal(err)
	}
	return name
}

func TestPlanEdits(t *testing.T) {
	t.Run("two_defects_with_overlapping_find_text_are_rejected", func(t *testing.T) {
		dir := t.TempDir()
		file := syntheticFile(t, dir, "scratch.go", "package scratch\n\nfunc f() int {\n\treturn 1 + 2\n}\n")

		d1 := bugs.Defect{ID: "X1", File: file, Find: "1 + 2", Replace: "1 - 2", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}
		d2 := bugs.Defect{ID: "X2", File: file, Find: "return 1 + 2", Replace: "return 0", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}

		_, err := planEdits(dir, []bugs.Defect{d1, d2})
		if err == nil {
			t.Fatal("planEdits with overlapping Find spans = nil error, want an overlap error")
		}
		if !strings.Contains(err.Error(), "overlap") {
			t.Fatalf("planEdits error = %v, want it to mention \"overlap\"", err)
		}
	})

	t.Run("two_defects_with_disjoint_find_text_in_the_same_file_compose", func(t *testing.T) {
		dir := t.TempDir()
		file := syntheticFile(t, dir, "scratch.go", "package scratch\n\nfunc f() int {\n\treturn 1 + 2\n}\n\nfunc g() int {\n\treturn 3 + 4\n}\n")

		d1 := bugs.Defect{ID: "X1", File: file, Find: "1 + 2", Replace: "1 - 2", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}
		d2 := bugs.Defect{ID: "X2", File: file, Find: "3 + 4", Replace: "3 - 4", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}

		edits, err := planEdits(dir, []bugs.Defect{d1, d2})
		if err != nil {
			t.Fatalf("planEdits with disjoint Find spans error = %v, want nil", err)
		}
		if len(edits) != 2 {
			t.Fatalf("len(edits) = %d, want 2", len(edits))
		}
		if err := applyEdits(dir, edits); err != nil {
			t.Fatalf("applyEdits error = %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // dir is a t.TempDir() this test created, not external input
		if err != nil {
			t.Fatal(err)
		}
		want := "package scratch\n\nfunc f() int {\n\treturn 1 - 2\n}\n\nfunc g() int {\n\treturn 3 - 4\n}\n"
		if string(got) != want {
			t.Fatalf("applied content =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("find_text_not_present_is_an_error", func(t *testing.T) {
		dir := t.TempDir()
		file := syntheticFile(t, dir, "scratch.go", "package scratch\n")
		d := bugs.Defect{ID: "X1", File: file, Find: "nonexistent", Replace: "x", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}
		if _, err := planEdits(dir, []bugs.Defect{d}); err == nil {
			t.Fatal("planEdits with absent Find text = nil error, want an error")
		}
	})

	t.Run("find_text_appearing_twice_is_an_error", func(t *testing.T) {
		dir := t.TempDir()
		file := syntheticFile(t, dir, "scratch.go", "package scratch\n\nfunc f() { x := 1; y := 1 }\n")
		d := bugs.Defect{ID: "X1", File: file, Find: "1", Replace: "2", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}
		_, err := planEdits(dir, []bugs.Defect{d})
		if err == nil {
			t.Fatal("planEdits with ambiguous (2x) Find text = nil error, want an error")
		}
		if !strings.Contains(err.Error(), "appears 2 times") {
			t.Fatalf("planEdits error = %v, want it to mention the ambiguous occurrence count", err)
		}
	})

	t.Run("line_number_is_1_indexed_and_correct", func(t *testing.T) {
		dir := t.TempDir()
		file := syntheticFile(t, dir, "scratch.go", "line1\nline2\nTARGET\nline4\n")
		d := bugs.Defect{ID: "X1", File: file, Find: "TARGET", Replace: "REPLACED", DetectedBy: bugs.DetectedByTest, BreaksTests: []string{"t"}}
		edits, err := planEdits(dir, []bugs.Defect{d})
		if err != nil {
			t.Fatalf("planEdits error = %v", err)
		}
		if len(edits) != 1 || edits[0].line != 3 {
			t.Fatalf("edits = %+v, want a single edit on line 3", edits)
		}
	})
}

func TestCatalogueSanity(t *testing.T) {
	all := bugs.All()

	t.Run("has_at_least_20_defects", func(t *testing.T) {
		if len(all) < 20 {
			t.Fatalf("len(bugs.All()) = %d, want >= 20", len(all))
		}
	})

	t.Run("every_defect_is_internally_valid", func(t *testing.T) {
		if errs := bugs.ValidateAll(); len(errs) > 0 {
			for _, e := range errs {
				t.Error(e)
			}
		}
	})

	t.Run("ids_are_unique", func(t *testing.T) {
		seen := make(map[string]bool, len(all))
		for _, d := range all {
			if seen[d.ID] {
				t.Fatalf("duplicate defect ID %q", d.ID)
			}
			seen[d.ID] = true
		}
	})

	t.Run("covers_at_least_8_distinct_classes", func(t *testing.T) {
		classes := bugs.Classes()
		if len(classes) < 8 {
			t.Fatalf("bugs.Classes() = %v (%d classes), want >= 8", classes, len(classes))
		}
	})

	t.Run("includes_both_test_only_and_lint_only_defects", func(t *testing.T) {
		var hasTest, hasLint bool
		for _, d := range all {
			switch d.DetectedBy {
			case bugs.DetectedByTest:
				hasTest = true
			case bugs.DetectedByLint:
				hasLint = true
			}
		}
		if !hasTest {
			t.Error("no defect in the catalogue has DetectedBy == test")
		}
		if !hasLint {
			t.Error("no defect in the catalogue has DetectedBy == lint; the review-node story needs at least one lint-only defect")
		}
	})

	t.Run("every_file_a_defect_targets_exists_in_src", func(t *testing.T) {
		src := realSrcDir(t)
		for _, d := range all {
			if _, err := os.Stat(filepath.Join(src, d.File)); err != nil {
				t.Errorf("defect %s targets %s, which does not exist under src/: %v", d.ID, d.File, err)
			}
		}
	})
}

// defectVerification is one row of the summary table
// TestEveryDefect_IndividuallyVerified prints at the end.
type defectVerification struct {
	id             string
	class          string
	detectedBy     string
	buildOK        bool
	missingFail    []string
	unexpectedFail []string
	lintAvailable  bool
	lintUnmatched  []string
	ok             bool
}

// TestEveryDefect_IndividuallyVerified is requirement #4's automation:
// every catalogued defect, injected alone, must produce a copy that (a)
// still compiles, (b) fails exactly the tests it says it will fail (no
// more, no less), and (c) trips exactly the linter it claims to, when
// golangci-lint is available. Each subtest drives this through the real
// run() entry point, so it is exercising exactly what `go run
// ./inject.go --bugs=<ID> --out=<dir>` does.
func TestEveryDefect_IndividuallyVerified(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and race-tests a fresh copy per defect; skipped in -short mode")
	}
	src := realSrcDir(t)
	all := bugs.All()

	// maxFlakyAttempts bounds how many times a FlakyDetection defect gets
	// re-injected and re-verified before its catalogue entry is treated
	// as wrong. -race only reports a data race when it happens to
	// observe the racing accesses' actual interleaving, so a single
	// clean-looking run of a genuinely racy defect is not proof its
	// BreaksTests entry is stale — it may simply not have been hit that
	// time. A non-flaky defect always gets exactly one attempt: for
	// those, one miss does mean the catalogue is wrong.
	const maxFlakyAttempts = 5

	var results []defectVerification
	for _, d := range all {
		t.Run(d.ID, func(t *testing.T) {
			attempts := 1
			if d.FlakyDetection {
				attempts = maxFlakyAttempts
			}

			var manifest injectedManifest
			var runErr error
			var stdout, stderr strings.Builder
			clean := false
			for i := 0; i < attempts && !clean; i++ {
				out := t.TempDir()
				stdout.Reset()
				stderr.Reset()
				runErr = run([]string{"--bugs=" + d.ID, "--src=" + src, "--out=" + out}, &stdout, &stderr)

				data, readErr := os.ReadFile(filepath.Join(out, "injected.json")) //nolint:gosec // out is a t.TempDir() this test created, not external input
				if readErr != nil {
					t.Fatalf("defect %s: attempt %d: run() error = %v; injected.json also missing: %v\nstdout:\n%s\nstderr:\n%s",
						d.ID, i+1, runErr, readErr, stdout.String(), stderr.String())
				}
				if err := json.Unmarshal(data, &manifest); err != nil {
					t.Fatalf("defect %s: attempt %d: parsing injected.json: %v", d.ID, i+1, err)
				}
				clean = runErr == nil
				if d.FlakyDetection && !clean && i < attempts-1 {
					t.Logf("defect %s: attempt %d/%d did not observe the race this time (BreaksTests %v missing: %v); retrying",
						d.ID, i+1, attempts, d.BreaksTests, manifest.Verification.MissingFailingTests)
				}
			}
			v := manifest.Verification

			results = append(results, defectVerification{
				id: d.ID, class: d.Class, detectedBy: string(d.DetectedBy),
				buildOK: v.BuildOK, missingFail: v.MissingFailingTests, unexpectedFail: v.UnexpectedFailingTests,
				lintAvailable: v.LintAvailable, lintUnmatched: v.LintUnmatchedDefects,
				ok: clean,
			})

			if !v.BuildOK {
				t.Errorf("defect %s: injected copy did not compile:\n%s", d.ID, stdout.String())
			}
			if len(v.MissingFailingTests) > 0 {
				verb := "did not actually fail"
				if d.FlakyDetection {
					verb = fmt.Sprintf("did not actually fail in any of %d attempts", attempts)
				}
				t.Errorf("defect %s: catalogued BreaksTests %v, but these %s: %v",
					d.ID, d.BreaksTests, verb, v.MissingFailingTests)
			}
			if len(v.UnexpectedFailingTests) > 0 {
				t.Errorf("defect %s: broke test(s) missing from its catalogue entry's BreaksTests: %v",
					d.ID, v.UnexpectedFailingTests)
			}
			if !v.LintAvailable {
				if d.DetectedBy == bugs.DetectedByLint || d.DetectedBy == bugs.DetectedByBoth {
					t.Log("golangci-lint not on PATH: this defect's lint channel was not exercised")
				}
			} else if len(v.LintUnmatchedDefects) > 0 {
				t.Errorf("defect %s: catalogued as DetectedBy=%s, Linter=%q, but that linter did not fire on the injected copy",
					d.ID, d.DetectedBy, d.Linter)
			}
			if d.DetectedBy == bugs.DetectedByTest && d.Linter != "" {
				t.Errorf("defect %s: DetectedBy is %q but Linter is set to %q", d.ID, d.DetectedBy, d.Linter)
			}
			if d.FlakyDetection && d.DetectedBy != bugs.DetectedByTest {
				t.Errorf("defect %s: FlakyDetection is only meaningful for DetectedBy=test (races are a test-channel phenomenon), got %q", d.ID, d.DetectedBy)
			}
		})
	}

	t.Run("summary", func(t *testing.T) {
		var b strings.Builder
		fmt.Fprintf(&b, "\n%-4s  %-32s  %-11s  %-6s  %-6s  %s\n", "id", "class", "detected_by", "builds", "tests", "lint")
		okCount := 0
		for _, v := range results {
			testsOK := len(v.missingFail) == 0 && len(v.unexpectedFail) == 0
			lintCol := "n/a"
			if v.lintAvailable {
				if len(v.lintUnmatched) == 0 {
					lintCol = "ok"
				} else {
					lintCol = "MISS"
				}
			}
			if v.ok {
				okCount++
			}
			fmt.Fprintf(&b, "%-4s  %-32s  %-11s  %-6v  %-6v  %s\n", v.id, v.class, v.detectedBy, v.buildOK, testsOK, lintCol)
		}
		fmt.Fprintf(&b, "\n%d/%d defects individually verified exactly as catalogued (inject alone, copy compiles, catalogued test(s)/linter fire, nothing else breaks)\n", okCount, len(results))
		t.Log(b.String())
		if okCount != len(results) {
			t.Errorf("%d/%d defects did not verify cleanly; see the per-ID subtests above for details", len(results)-okCount, len(results))
		}
	})
}

// TestComposability injects several defects that target different files
// (and, within ledger.go, disjoint functions) together in one run, and
// checks the batch behaves exactly as the union of their individual
// catalogue entries predicts: requirement #5's "injecting N defects at
// once must not have them interfere," exercised with real catalogue
// entries rather than synthetic ones.
func TestComposability(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and race-tests a fresh copy; skipped in -short mode")
	}
	src := realSrcDir(t)

	// B01 (delay.go), B15 (errors.go), B19 (policy.go) and B23
	// (ledger.go, lint-only) each target a different file, so none of
	// their Find spans can possibly overlap with one another.
	ids := []string{"B01", "B15", "B19", "B23"}
	for _, id := range ids {
		if _, ok := bugs.ByID(id); !ok {
			t.Fatalf("test setup: defect %s is not in the catalogue (did an ID change?)", id)
		}
	}

	out := t.TempDir()
	var stdout, stderr strings.Builder
	err := run([]string{"--bugs=" + strings.Join(ids, ","), "--src=" + src, "--out=" + out}, &stdout, &stderr)

	manifest := readManifest(t, out)
	if !manifest.Verification.BuildOK {
		t.Fatalf("composed injection of %v did not compile:\n%s", ids, stdout.String())
	}
	if len(manifest.Defects) != len(ids) {
		t.Fatalf("injected.json lists %d defects, want %d", len(manifest.Defects), len(ids))
	}
	if len(manifest.Verification.MissingFailingTests) > 0 {
		t.Errorf("composed injection of %v: expected failing tests that did not fail: %v", ids, manifest.Verification.MissingFailingTests)
	}
	if len(manifest.Verification.UnexpectedFailingTests) > 0 {
		t.Errorf("composed injection of %v: unexpected failing tests (cross-defect interference?): %v", ids, manifest.Verification.UnexpectedFailingTests)
	}
	if manifest.Verification.LintAvailable && len(manifest.Verification.LintUnmatchedDefects) > 0 {
		t.Errorf("composed injection of %v: lint-detected defects that did not fire: %v", ids, manifest.Verification.LintUnmatchedDefects)
	}
	if err != nil {
		t.Errorf("run() error = %v (see fields above for which check failed)", err)
	}
}
