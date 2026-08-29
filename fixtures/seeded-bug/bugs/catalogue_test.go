package bugs

import (
	"strings"
	"testing"
)

func validDefect() Defect {
	return Defect{
		ID:          "ZZ",
		Class:       "off-by-one",
		File:        "scratch.go",
		Symbol:      "F",
		Description: "a test fixture defect",
		Find:        "a",
		Replace:     "b",
		BreaksTests: []string{"TestF/case"},
		DetectedBy:  DetectedByTest,
	}
}

func TestDefect_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(Defect) Defect
		wantErr bool
	}{
		{"valid_test_detected_defect", func(d Defect) Defect { return d }, false},
		{"empty_id", func(d Defect) Defect { d.ID = ""; return d }, true},
		{"empty_class", func(d Defect) Defect { d.Class = ""; return d }, true},
		{"empty_file", func(d Defect) Defect { d.File = ""; return d }, true},
		{"file_contains_dotdot", func(d Defect) Defect { d.File = "../escape.go"; return d }, true},
		{"empty_symbol", func(d Defect) Defect { d.Symbol = ""; return d }, true},
		{"empty_description", func(d Defect) Defect { d.Description = ""; return d }, true},
		{"empty_find", func(d Defect) Defect { d.Find = ""; return d }, true},
		{"find_equals_replace_is_a_no_op", func(d Defect) Defect { d.Replace = d.Find; return d }, true},
		{"test_detected_with_empty_breaks_tests", func(d Defect) Defect { d.BreaksTests = nil; return d }, true},
		{"test_detected_with_linter_set", func(d Defect) Defect { d.Linter = "govet"; return d }, true},
		{"unknown_detected_by", func(d Defect) Defect { d.DetectedBy = Channel("bogus"); return d }, true},
		{
			"lint_detected_valid", func(d Defect) Defect {
				d.DetectedBy = DetectedByLint
				d.BreaksTests = nil
				d.Linter = "govet"
				return d
			}, false,
		},
		{
			"lint_detected_with_breaks_tests_set", func(d Defect) Defect {
				d.DetectedBy = DetectedByLint
				d.Linter = "govet"
				return d
			}, true,
		},
		{
			"lint_detected_without_linter", func(d Defect) Defect {
				d.DetectedBy = DetectedByLint
				d.BreaksTests = nil
				return d
			}, true,
		},
		{
			"both_detected_valid", func(d Defect) Defect {
				d.DetectedBy = DetectedByBoth
				d.Linter = "errcheck"
				return d
			}, false,
		},
		{
			"both_detected_without_linter", func(d Defect) Defect {
				d.DetectedBy = DetectedByBoth
				return d
			}, true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := tc.mutate(validDefect()).Validate()
			if tc.wantErr && len(errs) == 0 {
				t.Fatal("Validate() = no errors, want at least one")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("Validate() = %v, want no errors", errs)
			}
		})
	}
}

func TestRegister_PanicsOnDuplicateID(t *testing.T) {
	saved := registry
	registry = nil
	t.Cleanup(func() { registry = saved })

	register(validDefect())

	defer func() {
		if recover() == nil {
			t.Fatal("register() with a duplicate ID did not panic, want a panic")
		}
	}()
	register(validDefect())
}

func TestAll_IsSortedAndIsACopy(t *testing.T) {
	all := All()
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatalf("All() is not strictly sorted by ID at index %d: %q then %q", i, all[i-1].ID, all[i].ID)
		}
	}
	if len(all) > 0 {
		all[0].ID = "MUTATED"
		if All()[0].ID == "MUTATED" {
			t.Fatal("mutating All()'s result affected a later call; All() must return an independent copy")
		}
	}
}

func TestByID(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("no defects registered; ByID cannot be meaningfully tested")
	}
	want := all[0]

	t.Run("known_id_is_found", func(t *testing.T) {
		got, ok := ByID(want.ID)
		if !ok || got.ID != want.ID {
			t.Fatalf("ByID(%q) = (%+v, %v), want (id=%s, true)", want.ID, got, ok, want.ID)
		}
	})

	t.Run("unknown_id_is_not_found", func(t *testing.T) {
		if _, ok := ByID("NO-SUCH-ID"); ok {
			t.Fatal("ByID(\"NO-SUCH-ID\") = true, want false")
		}
	})
}

func TestClasses_IsSortedAndDeduplicated(t *testing.T) {
	classes := Classes()
	seen := make(map[string]bool, len(classes))
	for i, c := range classes {
		if seen[c] {
			t.Fatalf("Classes() contains duplicate class %q", c)
		}
		seen[c] = true
		if i > 0 && classes[i-1] >= c {
			t.Fatalf("Classes() is not strictly sorted at index %d: %q then %q", i, classes[i-1], c)
		}
	}
}

func TestValidateAll_ReportsEveryProblem(t *testing.T) {
	saved := registry
	registry = nil
	t.Cleanup(func() { registry = saved })

	register(validDefect())
	broken := validDefect()
	broken.ID = "ZZBROKEN"
	broken.Description = ""
	register(broken)

	errs := ValidateAll()
	if len(errs) != 1 {
		t.Fatalf("ValidateAll() = %v (%d errors), want exactly 1", errs, len(errs))
	}
	if !strings.Contains(errs[0].Error(), "ZZBROKEN") {
		t.Fatalf("ValidateAll() error = %v, want it to name defect ZZBROKEN", errs[0])
	}
}
