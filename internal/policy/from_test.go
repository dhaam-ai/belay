package policy_test

import (
	"testing"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/google/go-cmp/cmp"
)

func TestFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   config.Policy
		want policy.Policy
	}{
		{name: "nothing configured is nothing pinned", in: config.Policy{}, want: policy.Policy{}},
		{
			name: "every rule",
			in: config.Policy{
				ForbiddenPaths:    []string{"infra/**"},
				MaxChangedFiles:   25,
				MaxChangedLines:   2000,
				NoNewDependencies: true,
				RequiredTests:     []config.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
				ForbiddenContent:  []config.ContentRule{{Pattern: "AKIA", Paths: []string{"**/*.go"}, Reason: "a key"}},
			},
			want: policy.Policy{
				ForbiddenPaths:    []string{"infra/**"},
				MaxChangedFiles:   25,
				MaxChangedLines:   2000,
				NoNewDependencies: true,
				RequiredTests:     []policy.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
				ForbiddenContent:  []policy.ContentRule{{Pattern: "AKIA", Paths: []string{"**/*.go"}, Reason: "a key"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(tt.want, policy.From(tt.in)); diff != "" {
				t.Errorf("From (-want +got):\n%s", diff)
			}
		})
	}
}

// A pin is written into manifest.json and read back on every resume, so it
// must not alias the configuration the caller still holds: a later belay that
// edited the live config in place would otherwise edit the run's pinned rules
// with it.
func TestFromCopiesEveryList(t *testing.T) {
	t.Parallel()

	in := config.Policy{
		ForbiddenPaths:   []string{"infra/**"},
		RequiredTests:    []config.TestRule{{When: "**/*.go", Require: "**/*_test.go"}},
		ForbiddenContent: []config.ContentRule{{Pattern: "AKIA", Paths: []string{"**/*.go"}}},
	}
	got := policy.From(in)
	in.ForbiddenPaths[0] = "elsewhere/**"
	in.RequiredTests[0].When = "**/*.py"
	in.ForbiddenContent[0].Paths[0] = "**/*.py"

	if got.ForbiddenPaths[0] != "infra/**" {
		t.Errorf("forbidden_paths aliased the configuration: %q", got.ForbiddenPaths[0])
	}
	if got.RequiredTests[0].When != "**/*.go" {
		t.Errorf("required_tests aliased the configuration: %q", got.RequiredTests[0].When)
	}
	if got.ForbiddenContent[0].Paths[0] != "**/*.go" {
		t.Errorf("forbidden_content paths aliased the configuration: %q", got.ForbiddenContent[0].Paths[0])
	}
}

// Every field of the configuration section must reach the pin. A rule a
// person wrote that the pin quietly drops is a guardrail that does nothing,
// which is the failure this whole slice exists to prevent.
func TestFromCarriesEveryConfiguredRule(t *testing.T) {
	t.Parallel()

	full := config.Policy{
		ForbiddenPaths:    []string{"a/**"},
		MaxChangedFiles:   1,
		MaxChangedLines:   1,
		NoNewDependencies: true,
		RequiredTests:     []config.TestRule{{When: "a", Require: "b"}},
		ForbiddenContent:  []config.ContentRule{{Pattern: "x"}},
	}
	if policy.From(full).Empty() {
		t.Fatal("From dropped every rule it was given")
	}
	if !policy.From(config.Policy{}).Empty() {
		t.Error("From invented a rule out of an empty configuration")
	}
}
