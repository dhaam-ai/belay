package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/policy"
	"github.com/google/go-cmp/cmp"
)

func TestSurvey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files map[string]string
		want  []policy.Dependency
	}{
		{
			name:  "a workspace that declares nothing",
			files: map[string]string{"main.go": "package main\n"},
			want:  []policy.Dependency{},
		},
		{
			name: "a go.mod require block, without its versions or its comments",
			files: map[string]string{"go.mod": "module example.com/x\n\ngo 1.26\n\n" +
				"require (\n\tgithub.com/spf13/cobra v1.8.0\n\t// a comment\n\tgopkg.in/yaml.v3 v3.0.1 // indirect\n)\n"},
			want: []policy.Dependency{
				{Manifest: "go.mod", Name: "github.com/spf13/cobra"},
				{Manifest: "go.mod", Name: "gopkg.in/yaml.v3"},
			},
		},
		{
			name:  "a single-line go.mod require",
			files: map[string]string{"go.mod": "module x\n\nrequire github.com/pkg/errors v0.9.1\n"},
			want:  []policy.Dependency{{Manifest: "go.mod", Name: "github.com/pkg/errors"}},
		},
		{
			name: "replace, exclude and toolchain are not dependencies",
			files: map[string]string{"go.mod": "module x\n\ntoolchain go1.26.0\n\n" +
				"replace github.com/a/b => ../b\n\nexclude github.com/c/d v1.0.0\n"},
			want: []policy.Dependency{},
		},
		{
			name: "every kind of package.json dependency",
			files: map[string]string{"package.json": `{"name":"x",` +
				`"dependencies":{"react":"^18"},` +
				`"devDependencies":{"vitest":"^1"},` +
				`"peerDependencies":{"typescript":"^5"},` +
				`"optionalDependencies":{"fsevents":"^2"}}`},
			want: []policy.Dependency{
				{Manifest: "package.json", Name: "fsevents"},
				{Manifest: "package.json", Name: "react"},
				{Manifest: "package.json", Name: "typescript"},
				{Manifest: "package.json", Name: "vitest"},
			},
		},
		{
			name: "requirements.txt, normalized as PEP 503 says",
			files: map[string]string{"requirements.txt": "# a comment\n-r other.txt\n--index-url https://x\n" +
				"Flask_SQLAlchemy==3.0.1\nrequests>=2,<3\nhttpx[http2]\n\n"},
			want: []policy.Dependency{
				{Manifest: "requirements.txt", Name: "flask-sqlalchemy"},
				{Manifest: "requirements.txt", Name: "httpx"},
				{Manifest: "requirements.txt", Name: "requests"},
			},
		},
		{
			name: "two manifests in a monorepo are two places",
			files: map[string]string{
				"api/go.mod":       "module api\n\nrequire github.com/x/y v1.0.0\n",
				"web/package.json": `{"dependencies":{"react":"^18"}}`,
			},
			want: []policy.Dependency{
				{Manifest: "api/go.mod", Name: "github.com/x/y"},
				{Manifest: "web/package.json", Name: "react"},
			},
		},
		{
			name: "a manifest belay never treats as part of the project",
			files: map[string]string{
				"go.mod":                             "module x\n",
				"node_modules/left-pad/package.json": `{"dependencies":{"sneaky":"^1"}}`,
				"vendor/github.com/a/go.mod":         "module a\n\nrequire github.com/sneaky/b v1.0.0\n",
				".git/package.json":                  `{"dependencies":{"sneaky":"^1"}}`,
			},
			want: []policy.Dependency{},
		},
		{
			name:  "pyproject.toml is out of scope and says nothing",
			files: map[string]string{"pyproject.toml": "[project]\ndependencies = [\"requests\"]\n"},
			want:  []policy.Dependency{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := policy.Survey(context.Background(), tree(t, tt.files))
			if err != nil {
				t.Fatalf("Survey: %v", err)
			}
			if got.Dependencies == nil {
				t.Error("Baseline.Dependencies is nil; a workspace that declares nothing must still serialize as []")
			}
			if diff := cmp.Diff(tt.want, got.Dependencies); diff != "" {
				t.Errorf("Survey (-want +got):\n%s", diff)
			}
		})
	}
}

// A baseline missing what a manifest declares would make every one of those
// dependencies look new at the gate, and the run would be stopped over a
// dependency that was there all along. Refusing to survey is the only answer
// that cannot do that.
func TestSurveyRefusesAManifestItCannotRead(t *testing.T) {
	t.Parallel()

	_, err := policy.Survey(context.Background(), tree(t, map[string]string{
		"package.json": "{ this is not json",
	}))
	if err == nil {
		t.Fatal("Survey of an unparseable package.json returned no error")
	}
	if !strings.Contains(err.Error(), "package.json") {
		t.Errorf("Survey error = %v; it must name the file that could not be read", err)
	}
}

func TestSurveyIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()

	got, err := policy.Survey(context.Background(), tree(t, map[string]string{
		"package.json": `{"dependencies":{"zod":"^3","axios":"^1"},"devDependencies":{"zod":"^3"}}`,
	}))
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	want := []policy.Dependency{
		{Manifest: "package.json", Name: "axios"},
		{Manifest: "package.json", Name: "zod"},
	}
	if diff := cmp.Diff(want, got.Dependencies); diff != "" {
		t.Errorf("Survey (-want +got):\n%s", diff)
	}
}
