//go:build fixrate

package fixrate

import (
	"errors"
	"testing"
)

func TestSelection_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sel     Selection
		wantErr bool
	}{
		{"bugs only", Selection{Bugs: []string{"B01"}}, false},
		{"random with seed", Selection{Random: 5, Seed: 42}, false},
		{"random is zero and seed set is still invalid without bugs", Selection{Seed: 42}, true},
		{"both bugs and random", Selection{Bugs: []string{"B01"}, Random: 3}, true},
		{"neither", Selection{}, true},
		{"negative random", Selection{Random: -1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.sel.validate()
			if tt.wantErr && err == nil {
				t.Fatalf("validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			if tt.wantErr && !errors.Is(err, ErrInvalidSelection) {
				t.Errorf("validate() = %v, want it to wrap ErrInvalidSelection", err)
			}
		})
	}
}

func TestSelection_Args(t *testing.T) {
	t.Parallel()

	if got, want := (Selection{Bugs: []string{"B01", "B05"}}).args(), []string{"--bugs=B01,B05"}; !equalStrings(got, want) {
		t.Errorf("args() = %v, want %v", got, want)
	}
	if got, want := (Selection{Random: 5, Seed: 42}).args(), []string{"--random=5", "--seed=42"}; !equalStrings(got, want) {
		t.Errorf("args() = %v, want %v", got, want)
	}
	if got, want := (Selection{Random: 5, Seed: -7}).args(), []string{"--random=5", "--seed=-7"}; !equalStrings(got, want) {
		t.Errorf("args() = %v, want %v", got, want)
	}
}

func TestSelection_Label_IsStableAndFilesystemSafe(t *testing.T) {
	t.Parallel()

	labels := map[string]bool{}
	for _, sel := range []Selection{
		{Bugs: []string{"B01", "B05"}},
		{Bugs: []string{"B01"}},
		{Random: 5, Seed: 42},
		{Random: 5, Seed: 43},
		{Random: 6, Seed: 42},
	} {
		l := sel.label()
		if l == "" {
			t.Fatalf("label() for %+v is empty", sel)
		}
		for _, r := range l {
			safe := r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !safe {
				t.Fatalf("label() = %q contains unsafe rune %q", l, r)
			}
		}
		if labels[l] {
			t.Fatalf("label() = %q collides with another distinct Selection", l)
		}
		labels[l] = true
	}
}

// injectDefects against a bogus defect ID must fail loudly, with the
// selection preserved on the error, rather than silently producing a
// smaller-than-requested defect set.
func TestInjectDefects_UnknownID(t *testing.T) {
	t.Parallel()
	fixtureDir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}

	_, err = injectDefects(t.Context(), fixtureDir, t.TempDir(), Selection{Bugs: []string{"B999"}})
	if err == nil {
		t.Fatal("injectDefects() = nil error, want one for an unknown defect id")
	}
	var injectErr *InjectError
	if !errors.As(err, &injectErr) {
		t.Fatalf("injectDefects() error = %v (%T), want *InjectError", err, err)
	}
	if injectErr.Manifest != nil {
		t.Errorf("InjectError.Manifest = %+v, want nil (the process should fail before writing one for an unresolvable ID)", injectErr.Manifest)
	}
}

func TestDefaultFixtureDir_LooksRight(t *testing.T) {
	t.Parallel()
	dir, err := DefaultFixtureDir()
	if err != nil {
		t.Fatalf("DefaultFixtureDir() error = %v", err)
	}
	if !looksLikeFixtureDir(dir) {
		t.Errorf("DefaultFixtureDir() = %q, does not look like the fixture", dir)
	}
}
