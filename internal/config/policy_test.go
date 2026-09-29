package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// loadPolicy merges doc onto the defaults and returns the policy section.
func loadPolicy(t *testing.T, doc string) Policy {
	t.Helper()
	cfg := Default()
	if err := cfg.Merge([]byte(doc), "test"); err != nil {
		t.Fatalf("merge:\n%s\n%v", doc, err)
	}
	return cfg.Policy
}

func TestPolicyDefaultsToNoRulesAtAll(t *testing.T) {
	t.Parallel()

	got := Default().Policy
	if diff := cmp.Diff(Policy{}, got); diff != "" {
		t.Errorf("Default().Policy (-want +got):\n%s", diff)
	}
}

func TestPolicyDecodesEveryRule(t *testing.T) {
	t.Parallel()

	got := loadPolicy(t, `
version: 1
policy:
  forbidden_paths:
    - "infra/**"
    - ".github/workflows/*"
  max_changed_files: 25
  max_changed_lines: 2000
  no_new_dependencies: true
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
  forbidden_content:
    - pattern: "AKIA[0-9A-Z]{16}"
      reason: "an AWS access key id"
      paths: ["**/*.go"]
`)
	want := Policy{
		ForbiddenPaths:    []string{"infra/**", ".github/workflows/*"},
		MaxChangedFiles:   25,
		MaxChangedLines:   2000,
		NoNewDependencies: true,
		RequiredTests:     []TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
		ForbiddenContent: []ContentRule{
			{Pattern: "AKIA[0-9A-Z]{16}", Reason: "an AWS access key id", Paths: []string{"**/*.go"}},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("policy (-want +got):\n%s", diff)
	}
}

// A later layer wins a whole field, as every other setting does. A policy
// merged list by list would be a rule set nobody could read out of either
// file: the project's belay.yaml would silently inherit forbidden paths from
// a personal config that nobody else on the team has.
func TestALaterLayerReplacesAPolicyListOutright(t *testing.T) {
	t.Parallel()

	var cfg Config
	if err := cfg.Merge([]byte("version: 1\npolicy:\n  forbidden_paths: [\"a/**\", \"b/**\", \"c/**\"]\n"), "user"); err != nil {
		t.Fatalf("merge the user layer: %v", err)
	}
	if err := cfg.Merge([]byte("policy:\n  forbidden_paths: [\"z/**\"]\n"), "project"); err != nil {
		t.Fatalf("merge the project layer: %v", err)
	}
	if diff := cmp.Diff([]string{"z/**"}, cfg.Policy.ForbiddenPaths); diff != "" {
		t.Errorf("forbidden_paths after merging (-want +got):\n%s", diff)
	}
}

func TestPolicyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "a forbidden path belay cannot match",
			doc:  "policy:\n  forbidden_paths: [\"../outside/**\"]\n",
			want: "policy.forbidden_paths",
		},
		{
			name: "an empty forbidden path",
			doc:  "policy:\n  forbidden_paths: [\"\"]\n",
			want: "policy.forbidden_paths",
		},
		{
			name: "a negative file ceiling",
			doc:  "policy:\n  max_changed_files: -1\n",
			want: "policy.max_changed_files",
		},
		{
			name: "a negative line ceiling",
			doc:  "policy:\n  max_changed_lines: -1\n",
			want: "policy.max_changed_lines",
		},
		{
			name: "a required-tests rule with no when",
			doc:  "policy:\n  required_tests:\n    - require: \"**/*_test.go\"\n",
			want: "policy.required_tests[0].when",
		},
		{
			name: "a required-tests rule with no require",
			doc:  "policy:\n  required_tests:\n    - when: \"**/*.go\"\n",
			want: "policy.required_tests[0].require",
		},
		{
			name: "a required-tests glob belay cannot match",
			doc:  "policy:\n  required_tests:\n    - when: \"src**/x\"\n      require: \"**/*_test.go\"\n",
			want: "policy.required_tests[0].when",
		},
		{
			name: "a forbidden-content rule with no pattern",
			doc:  "policy:\n  forbidden_content:\n    - reason: \"nope\"\n",
			want: "policy.forbidden_content[0].pattern",
		},
		{
			name: "a forbidden-content pattern that is not a regular expression",
			doc:  "policy:\n  forbidden_content:\n    - pattern: \"a(b\"\n",
			want: "policy.forbidden_content[0].pattern",
		},
		{
			name: "a forbidden-content path glob belay cannot match",
			doc:  "policy:\n  forbidden_content:\n    - pattern: \"x\"\n      paths: [\"/etc/**\"]\n",
			want: "policy.forbidden_content[0].paths",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			if err := cfg.Merge([]byte(tt.doc), "test"); err != nil {
				t.Fatalf("merge: %v", err)
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s; a rule belay cannot honour is a guardrail that silently does nothing", tt.name)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate error = %v, want one wrapping ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate error = %v; it must name %s, so a person knows which line to fix", err, tt.want)
			}
		})
	}
}

func TestAWholePolicyValidates(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if err := cfg.Merge([]byte(`
policy:
  forbidden_paths: ["infra/**"]
  max_changed_files: 25
  max_changed_lines: 2000
  no_new_dependencies: true
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
  forbidden_content:
    - pattern: "AKIA[0-9A-Z]{16}"
      paths: ["**/*.go"]
`), "test"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a policy it should accept: %v", err)
	}
}

// Every setting a run was started under is recorded in its manifest, so a
// resumed run can report the drift. A policy left out of the snapshot would
// make a run that had its guardrails removed mid-flight look unchanged.
func TestPolicyIsInTheSnapshot(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if err := cfg.Merge([]byte(`
policy:
  forbidden_paths: ["infra/**", "b/**"]
  max_changed_files: 25
  no_new_dependencies: true
  required_tests:
    - when: "**/*.go"
      require: "**/*_test.go"
  forbidden_content:
    - pattern: "AKIA"
      paths: ["**/*.go"]
`), "test"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got := cfg.Snapshot()
	for key, want := range map[string]string{
		"policy.forbidden_paths":     "infra/**, b/**",
		"policy.max_changed_files":   "25",
		"policy.max_changed_lines":   "0",
		"policy.no_new_dependencies": "true",
		"policy.required_tests":      "**/*.go requires **/*_test.go",
		"policy.forbidden_content":   "AKIA in **/*.go",
	} {
		if got[key] != want {
			t.Errorf("Snapshot()[%q] = %q, want %q", key, got[key], want)
		}
	}
	if Default().Snapshot()["policy.forbidden_paths"] != "" {
		t.Error("a run with no policy must snapshot an empty value, so its digest is what it always was")
	}
}
