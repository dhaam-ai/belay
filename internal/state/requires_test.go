package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// requiresDocument is one of the two files that carries a requires list,
// with just enough of a loader and a document template to exercise it.
type requiresDocument struct {
	file string
	load func(path string) error
	// doc renders a minimal document at the current schema version. extra
	// is spliced in verbatim before the closing brace, so a case can add a
	// requires key, leave it out, or add a field in a shape this binary
	// cannot decode.
	doc func(extra string) string
}

func requiresDocuments() []requiresDocument {
	return []requiresDocument{
		{
			file: "state.json",
			load: func(path string) error {
				_, err := LoadState(path)
				return err
			},
			doc: func(extra string) string {
				return fmt.Sprintf(`{"schema_version":%d,"goal":"g"%s}`, StateSchemaVersion, extra)
			},
		},
		{
			file: "manifest.json",
			load: func(path string) error {
				_, err := LoadManifest(path)
				return err
			},
			doc: func(extra string) string {
				return fmt.Sprintf(`{"schema_version":%d,"run_id":"run-1"%s}`, ManifestSchemaVersion, extra)
			},
		},
	}
}

func writeRequiresDocument(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestLoad_RefusesARunThatRequiresAnUnknownCapability is the protection
// the requires list exists for: a document a later belay wrote with a
// control field this binary cannot honour must be refused by name, not
// decoded with that field silently dropped and later saved without it.
func TestLoad_RefusesARunThatRequiresAnUnknownCapability(t *testing.T) {
	cases := []struct {
		name     string
		requires string
		want     []string
	}{
		{name: "one unknown capability", requires: `["from.the.future"]`, want: []string{"from.the.future"}},
		{
			name:     "several, unsorted and repeated, are reported sorted and once each",
			requires: `["zeta","alpha","zeta"]`,
			want:     []string{"alpha", "zeta"},
		},
	}
	for _, d := range requiresDocuments() {
		for _, tt := range cases {
			t.Run(d.file+"/"+tt.name, func(t *testing.T) {
				path := writeRequiresDocument(t, d.file, d.doc(`,"requires":`+tt.requires))

				err := d.load(path)
				if err == nil {
					t.Fatalf("loading a %s that requires %s succeeded, want a refusal", d.file, tt.requires)
				}
				if !errors.Is(err, ErrUnknownCapability) {
					t.Errorf("error does not wrap ErrUnknownCapability: %v", err)
				}
				var capErr *CapabilityError
				if !errors.As(err, &capErr) {
					t.Fatalf("error is not a *CapabilityError: %v", err)
				}
				if capErr.File != d.file {
					t.Errorf("File = %q, want %q", capErr.File, d.file)
				}
				if diff := cmp.Diff(tt.want, capErr.Unknown); diff != "" {
					t.Errorf("Unknown mismatch (-want +got):\n%s", diff)
				}
				for _, name := range tt.want {
					if !strings.Contains(err.Error(), name) {
						t.Errorf("error %q does not name the capability %q", err, name)
					}
				}
			})
		}
	}
}

// TestLoad_ReportsTheMissingCapabilityBeforeAnyDecodeError covers a
// document whose unknown capability comes with a field in a shape this
// binary cannot decode. The capability is the real reason the document is
// unreadable, so it is what the error must say, rather than "damaged".
func TestLoad_ReportsTheMissingCapabilityBeforeAnyDecodeError(t *testing.T) {
	undecodable := map[string]string{
		"state.json":    `,"fix":"a future shape","requires":["from.the.future"]`,
		"manifest.json": `,"step":"a future shape","requires":["from.the.future"]`,
	}
	for _, d := range requiresDocuments() {
		t.Run(d.file, func(t *testing.T) {
			path := writeRequiresDocument(t, d.file, d.doc(undecodable[d.file]))

			err := d.load(path)
			var capErr *CapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("error = %v, want a *CapabilityError naming from.the.future", err)
			}
		})
	}
}

// TestLoad_AcceptsADocumentThatRequiresNothing guards the other direction:
// every way of saying "nothing required" must load, including a requires
// key that is absent, which is how every document written before the list
// existed looks.
func TestLoad_AcceptsADocumentThatRequiresNothing(t *testing.T) {
	cases := []struct {
		name  string
		extra string
	}{
		{name: "no requires key", extra: ``},
		{name: "requires is null", extra: `,"requires":null`},
		{name: "requires is empty", extra: `,"requires":[]`},
	}
	for _, d := range requiresDocuments() {
		for _, tt := range cases {
			t.Run(d.file+"/"+tt.name, func(t *testing.T) {
				path := writeRequiresDocument(t, d.file, d.doc(tt.extra))
				if err := d.load(path); err != nil {
					t.Errorf("load %s: %v", d.file, err)
				}
			})
		}
	}
}

// TestLoad_AcceptsARunThatRequiresBranchOutput is the other half of the
// refusal above: the capability this belay does have must load, or every
// branch-mode run it starts would be one it then refuses to resume.
func TestLoad_AcceptsARunThatRequiresBranchOutput(t *testing.T) {
	for _, d := range requiresDocuments() {
		t.Run(d.file, func(t *testing.T) {
			path := writeRequiresDocument(t, d.file, d.doc(`,"requires":["`+CapabilityOutputBranch+`"]`))
			if err := d.load(path); err != nil {
				t.Errorf("load a %s requiring %q: %v", d.file, CapabilityOutputBranch, err)
			}
		})
	}
}

func TestUnknownCapabilities(t *testing.T) {
	known := map[string]bool{"output.branch": true}
	tests := []struct {
		name     string
		requires []string
		want     []string
	}{
		{name: "nil requires nothing", requires: nil, want: nil},
		{name: "empty requires nothing", requires: []string{}, want: nil},
		{name: "a known capability is met", requires: []string{"output.branch"}, want: nil},
		{name: "an unknown capability is reported", requires: []string{"policy"}, want: []string{"policy"}},
		{
			name:     "only the unknown ones are reported, sorted, once each",
			requires: []string{"policy", "output.branch", "baseline", "policy"},
			want:     []string{"baseline", "policy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unknownCapabilities(tt.requires, known)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("unknownCapabilities(%q) mismatch (-want +got):\n%s", tt.requires, diff)
			}
		})
	}
}

func TestCapabilityError_NamesEachCapabilityAndSaysToUpgrade(t *testing.T) {
	tests := []struct {
		name string
		err  *CapabilityError
		want []string
	}{
		{
			name: "one capability",
			err:  &CapabilityError{File: "manifest.json", Unknown: []string{"output.branch"}},
			want: []string{"manifest.json", `"output.branch"`, "upgrade belay"},
		},
		{
			name: "two capabilities",
			err:  &CapabilityError{File: "state.json", Unknown: []string{"baseline", "plan.regenerate"}},
			want: []string{"state.json", `"baseline"`, `"plan.regenerate"`, "upgrade belay"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, want it to contain %q", msg, want)
				}
			}
		})
	}
}

// TestLoad_ReportsAnUnknownVersionWhateverItsRequiresLooksLike keeps the
// schema_version check first. A later schema is free to change what
// requires looks like, and a binary that cannot read that schema must say
// so, not report the document as damaged because requires did not decode.
func TestLoad_ReportsAnUnknownVersionWhateverItsRequiresLooksLike(t *testing.T) {
	tests := []struct {
		file   string
		load   func(path string) error
		doc    string
		target error
	}{
		{
			file: "state.json",
			load: func(path string) error {
				_, err := LoadState(path)
				return err
			},
			doc:    `{"schema_version":99,"requires":[{"name":"a later shape"}]}`,
			target: ErrUnsupportedStateVersion,
		},
		{
			file: "manifest.json",
			load: func(path string) error {
				_, err := LoadManifest(path)
				return err
			},
			doc:    `{"schema_version":99,"requires":[{"name":"a later shape"}]}`,
			target: ErrUnsupportedManifestVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			path := writeRequiresDocument(t, tt.file, tt.doc)
			if err := tt.load(path); !errors.Is(err, tt.target) {
				t.Errorf("error = %v, want one wrapping %v", err, tt.target)
			}
		})
	}
}

// TestVersionErrors_SayToUpgradeOnlyForANewerVersion keeps the two refusals
// a person meets for a run belay cannot read telling them what to do. A
// version above this binary's was written by a newer belay, and the only fix
// is to run that one, which the error must say as CapabilityError does. A
// version below it that no migration covers was not written by any belay
// this one can point to, so the error keeps saying only that the version is
// not supported.
func TestVersionErrors_SayToUpgradeOnlyForANewerVersion(t *testing.T) {
	type versionError interface {
		error
		Newer() bool
	}

	tests := []struct {
		name      string
		err       versionError
		wantNewer bool
		want      []string
		wantExact string
	}{
		{
			name:      "state.json from a newer belay",
			err:       &VersionError{Got: 99},
			wantNewer: true,
			want:      []string{"state.json", "99", "upgrade belay"},
		},
		{
			name:      "manifest.json from a newer belay",
			err:       &ManifestVersionError{Got: 99},
			wantNewer: true,
			want:      []string{"manifest.json", "99", "upgrade belay"},
		},
		{
			name:      "state.json at an older version nothing migrates",
			err:       &VersionError{Got: 0},
			wantExact: "state: state.json schema version 0 is not supported (want 2)",
		},
		{
			name:      "manifest.json at an older version nothing migrates",
			err:       &ManifestVersionError{Got: 0},
			wantExact: "state: manifest.json schema version 0 is not supported (want 2)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Newer(); got != tt.wantNewer {
				t.Errorf("Newer() = %v, want %v", got, tt.wantNewer)
			}
			msg := tt.err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Errorf("Error() = %q, want it to contain %q", msg, want)
				}
			}
			if tt.wantExact != "" && msg != tt.wantExact {
				t.Errorf("Error() = %q, want today's wording %q", msg, tt.wantExact)
			}
		})
	}
}
