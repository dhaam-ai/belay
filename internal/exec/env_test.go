//go:build unix

package exec

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var fakeParent = []string{
	"PATH=/usr/bin:/bin",
	"HOME=/home/belay",
	"TMPDIR=/tmp",
	"LANG=en_US.UTF-8",
	"TERM=xterm-256color",
	"SONAR_TOKEN=squ_parenttokenvalue0123456789abcdef012345",
	"ANTHROPIC_API_KEY=sk-ant-api03-parentkeyvalue0123456789",
	"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
	"GITHUB_TOKEN=ghp_parenttokenvalue0123456789abcdefgh",
	"DATABASE_URL=postgres://user:hunter2@db.internal:5432/prod",
	"CI=true",
	"UNRELATED_VAR=unrelated-value-that-must-not-leak",
	"MALFORMED",
	"=novalue",
}

func TestBuildEnvDeniesByDefault(t *testing.T) {
	t.Parallel()
	env, secrets, err := buildEnv(fakeParent, Command{})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	want := []string{
		"HOME=/home/belay",
		"LANG=en_US.UTF-8",
		"PATH=/usr/bin:/bin",
		"TERM=xterm-256color",
		"TMPDIR=/tmp",
	}
	if diff := cmp.Diff(want, env); diff != "" {
		t.Errorf("child environment mismatch (-want +got):\n%s", diff)
	}
	if len(secrets) != 0 {
		t.Errorf("base variables seeded the redactor: %v", secrets)
	}
}

func TestBuildEnvAllowlist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cmd         Command
		wantPresent []string
		wantAbsent  []string
		wantSecrets []string
	}{
		{
			name:        "scanner gets only its token",
			cmd:         Command{EnvAllow: []string{"SONAR_TOKEN"}},
			wantPresent: []string{"PATH=/usr/bin:/bin", "SONAR_TOKEN=squ_parenttokenvalue0123456789abcdef012345"},
			wantAbsent:  []string{"UNRELATED_VAR=unrelated-value-that-must-not-leak", "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY", "CI=true"},
			wantSecrets: []string{"SONAR_TOKEN"},
		},
		{
			name:        "agent gets only its key",
			cmd:         Command{EnvAllow: []string{"ANTHROPIC_API_KEY"}},
			wantPresent: []string{"ANTHROPIC_API_KEY=sk-ant-api03-parentkeyvalue0123456789"},
			wantAbsent:  []string{"SONAR_TOKEN=squ_parenttokenvalue0123456789abcdef012345", "GITHUB_TOKEN=ghp_parenttokenvalue0123456789abcdefgh"},
			wantSecrets: []string{"ANTHROPIC_API_KEY"},
		},
		{
			name:        "non-secret name is allowed without seeding the redactor",
			cmd:         Command{EnvAllow: []string{"CI"}},
			wantPresent: []string{"CI=true"},
			wantAbsent:  []string{"UNRELATED_VAR=unrelated-value-that-must-not-leak"},
		},
		{
			name:        "SecretEnv both allows and seeds",
			cmd:         Command{SecretEnv: []string{"DATABASE_URL"}},
			wantPresent: []string{"DATABASE_URL=postgres://user:hunter2@db.internal:5432/prod"},
			wantSecrets: []string{"DATABASE_URL"},
		},
		{
			name:        "absent allowlisted variable is skipped",
			cmd:         Command{EnvAllow: []string{"NOT_SET_ANYWHERE"}},
			wantAbsent:  []string{"NOT_SET_ANYWHERE="},
			wantPresent: []string{"PATH=/usr/bin:/bin"},
		},
		{
			name: "ExtraEnv overrides the parent value",
			cmd: Command{
				EnvAllow: []string{"SONAR_TOKEN"},
				//nolint:gosec // synthetic value, shaped like a token so the redactor sees it.
				ExtraEnv: map[string]string{"SONAR_TOKEN": "squ_overridden00000000000000000000000000", "PATH": "/sandbox/bin"},
			},
			wantPresent: []string{"SONAR_TOKEN=squ_overridden00000000000000000000000000", "PATH=/sandbox/bin"},
			wantAbsent:  []string{"SONAR_TOKEN=squ_parenttokenvalue0123456789abcdef012345", "PATH=/usr/bin:/bin"},
			wantSecrets: []string{"SONAR_TOKEN"},
		},
		{
			name:        "duplicate names appear once",
			cmd:         Command{EnvAllow: []string{"CI", "CI", "PATH"}},
			wantPresent: []string{"CI=true", "PATH=/usr/bin:/bin"},
		},
		{
			name:       "malformed parent entries are dropped",
			cmd:        Command{EnvAllow: []string{"MALFORMED"}},
			wantAbsent: []string{"MALFORMED=", "=novalue"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, secrets, err := buildEnv(fakeParent, tc.cmd)
			if err != nil {
				t.Fatalf("buildEnv: %v", err)
			}
			for _, want := range tc.wantPresent {
				if !slices.Contains(env, want) {
					t.Errorf("missing %q from child env %v", want, env)
				}
			}
			for _, bad := range tc.wantAbsent {
				if slices.Contains(env, bad) {
					t.Errorf("leaked %q into child env %v", bad, env)
				}
			}
			var labels []string
			for _, s := range secrets {
				labels = append(labels, s.Label)
			}
			slices.Sort(labels)
			want := slices.Clone(tc.wantSecrets)
			slices.Sort(want)
			if diff := cmp.Diff(want, labels, cmpEmptySlices()); diff != "" {
				t.Errorf("redactor seeds mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildEnvSortedAndWellFormed(t *testing.T) {
	t.Parallel()
	env, _, err := buildEnv(fakeParent, Command{EnvAllow: []string{"SONAR_TOKEN", "CI"}})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	if !slices.IsSorted(env) {
		t.Errorf("child env is not sorted, results will not be reproducible: %v", env)
	}
	for _, kv := range env {
		if !strings.Contains(kv, "=") {
			t.Errorf("entry %q is not NAME=value", kv)
		}
	}
}

func TestBuildEnvRejectsMalformedNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  Command
		want error
	}{
		{"empty allow name", Command{EnvAllow: []string{""}}, ErrInvalidEnv},
		{"equals in allow name", Command{EnvAllow: []string{"A=B"}}, ErrInvalidEnv},
		{"null in allow name", Command{EnvAllow: []string{"A\x00B"}}, ErrNullByte},
		{"empty extra name", Command{ExtraEnv: map[string]string{"": "x"}}, ErrInvalidEnv},
		{"equals in extra name", Command{ExtraEnv: map[string]string{"A=B": "x"}}, ErrInvalidEnv},
		{"null in extra value", Command{ExtraEnv: map[string]string{"A": "x\x00y"}}, ErrNullByte},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := buildEnv(fakeParent, tc.cmd); !errors.Is(err, tc.want) {
				t.Errorf("buildEnv error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestIsSecretName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"SONAR_TOKEN", true},
		{"ANTHROPIC_API_KEY", true},
		{"GITHUB_TOKEN", true},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"NPM_TOKEN", true},
		{"DB_PASSWORD", true},
		{"MY_PASSPHRASE", true},
		{"SSH_PRIVATE_KEY", true},
		{"GH_PAT", true},
		{"AUTH_HEADER", true},
		{"registry_credentials", true},
		// Fail-closed by design: a name that merely contains a credential word
		// is treated as one. A spurious marker in captured output is a far
		// cheaper mistake than a credential written to the run journal.
		{"NOT_A_SECRET", true},
		{"AUTHOR_NAME", true},
		{"BUILD_PROFILE", false},
		{"PATH", false},
		{"HOME", false},
		{"TMPDIR", false},
		{"LANG", false},
		{"TERM", false},
		{"CI", false},
		{"GOFLAGS", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSecretName(tc.name); got != tc.want {
				t.Errorf("IsSecretName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestBaseEnvNamesIsACopy stops a caller from widening the deny-by-default set
// for the whole process by mutating the returned slice.
func TestBaseEnvNamesIsACopy(t *testing.T) {
	t.Parallel()
	got := BaseEnvNames()
	got[0] = "AWS_SECRET_ACCESS_KEY"
	if slices.Contains(BaseEnvNames(), "AWS_SECRET_ACCESS_KEY") {
		t.Fatal("BaseEnvNames returned a slice aliasing package state")
	}
	if !slices.Contains(BaseEnvNames(), "PATH") {
		t.Error("PATH missing from BaseEnvNames")
	}
}

func cmpEmptySlices() cmp.Option {
	return cmp.FilterValues(func(x, y []string) bool {
		return len(x) == 0 && len(y) == 0
	}, cmp.Comparer(func(_, _ []string) bool { return true }))
}
