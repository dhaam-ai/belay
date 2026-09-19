//go:build unix

package git

import (
	"errors"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestParseVersion pins the shapes `git version` really prints. The string is
// not a clean semantic version: Apple, Windows and distribution builds all
// append their own suffix, and a parser that insists on three numbers and
// nothing else refuses the git most macOS developers have.
func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    Version
		wantErr bool
	}{
		{
			name: "Apple's build, which is what macOS ships",
			out:  "git version 2.54.0 (Apple Git-157)\n",
			want: Version{Major: 2, Minor: 54, Patch: 0},
		},
		{
			name: "a plain upstream build",
			out:  "git version 2.38.1\n",
			want: Version{Major: 2, Minor: 38, Patch: 1},
		},
		{
			name: "a Windows build with a fourth component",
			out:  "git version 2.50.1.windows.1\n",
			want: Version{Major: 2, Minor: 50, Patch: 1},
		},
		{
			name: "two components mean patch zero",
			out:  "git version 3.0\n",
			want: Version{Major: 3, Minor: 0, Patch: 0},
		},
		{
			name: "a release candidate keeps its numbers",
			out:  "git version 2.39.0.rc2\n",
			want: Version{Major: 2, Minor: 39, Patch: 0},
		},
		{
			name: "surrounding whitespace is not a parse failure",
			out:  "  git version 2.47.2  \n\n",
			want: Version{Major: 2, Minor: 47, Patch: 2},
		},
		{name: "empty output", out: "", wantErr: true},
		{name: "another program's version", out: "hg version 6.7\n", wantErr: true},
		{name: "the prefix with nothing after it", out: "git version \n", wantErr: true},
		{name: "a non-numeric major", out: "git version x.38.1\n", wantErr: true},
		{name: "a negative component", out: "git version 2.-1.0\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVersion(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseVersion(%q) = %v, want an error", tt.out, got)
				}
				if !errors.Is(err, belay.ErrToolchainMissing) {
					t.Errorf("errors.Is(err, belay.ErrToolchainMissing) = false for %v; an unreadable version is a broken toolchain, not a wrong answer", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion(%q) error = %v", tt.out, err)
			}
			if got != tt.want {
				t.Errorf("ParseVersion(%q) = %+v, want %+v", tt.out, got, tt.want)
			}
		})
	}
}

// TestVersionAtLeast pins the comparison MinVersion is used with. 2.38 is the
// floor because `merge-tree --write-tree` landed there, and that command is
// how this package answers "would this merge conflict?" without touching the
// working tree.
func TestVersionAtLeast(t *testing.T) {
	if MinVersion != (Version{Major: 2, Minor: 38}) {
		t.Fatalf("MinVersion = %+v, want 2.38.0: merge-tree --write-tree is the floor", MinVersion)
	}
	tests := []struct {
		name string
		have Version
		want bool
	}{
		{name: "exactly the minimum", have: Version{2, 38, 0}, want: true},
		{name: "a later patch", have: Version{2, 38, 4}, want: true},
		{name: "a later minor", have: Version{2, 54, 0}, want: true},
		{name: "a later major", have: Version{3, 0, 0}, want: true},
		{name: "one minor too old", have: Version{2, 37, 9}, want: false},
		{name: "much too old", have: Version{1, 9, 0}, want: false},
		{name: "the zero value is not a version", have: Version{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.have.AtLeast(MinVersion); got != tt.want {
				t.Errorf("%s.AtLeast(%s) = %t, want %t", tt.have, MinVersion, got, tt.want)
			}
		})
	}
}

// TestVersionString keeps the rendering that reaches an error message stable:
// a user comparing it with `git --version` should see the same three numbers.
func TestVersionString(t *testing.T) {
	if got, want := (Version{2, 38, 1}).String(), "2.38.1"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := MinVersion.String(), "2.38.0"; got != want {
		t.Errorf("MinVersion.String() = %q, want %q", got, want)
	}
}

// TestErrTooOldIsAToolchainProblem proves a too-old git is reported as a
// toolchain problem, not as a wrong answer about the repository. The CLI
// chooses in_place output on this error rather than telling the user their
// repository is unsupported.
func TestErrTooOldIsAToolchainProblem(t *testing.T) {
	if !errors.Is(ErrTooOld, belay.ErrToolchainMissing) {
		t.Errorf("errors.Is(ErrTooOld, belay.ErrToolchainMissing) = false, want true")
	}
	if errors.Is(ErrTooOld, belay.ErrUnsupported) {
		t.Errorf("errors.Is(ErrTooOld, belay.ErrUnsupported) = true; upgrading git fixes this, so it is not 'never'")
	}
}
