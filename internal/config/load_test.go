package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
)

// writeFile creates path inside a temporary tree and returns its directory.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadNoFilesGivesExactlyDefaults(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if diff := cmp.Diff(Default(), got); diff != "" {
		t.Errorf("absent config is not the defaults (-want +got):\n%s", diff)
	}
}

func TestLoadEmptyFileGivesExactlyDefaults(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "zero bytes", body: ""},
		{name: "only a newline", body: "\n"},
		{name: "only comments", body: "# nothing to see here\n"},
		{name: "explicit null document", body: "---\nnull\n"},
		{name: "empty sections", body: "agent:\ngraph:\nbudget:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, ProjectFile, tt.body)
			got, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if diff := cmp.Diff(Default(), got); diff != "" {
				t.Errorf("empty config is not the defaults (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMergeOverridesOnlyTheFieldsPresent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want func(*Config)
	}{
		{
			name: "one leaf keeps its siblings",
			body: "budget:\n  max_usd: 12.5\n",
			want: func(c *Config) { c.Budget.MaxUSD = 12.5 },
		},
		{
			name: "one leaf keeps every other section",
			body: "graph:\n  give_up: 7\n",
			want: func(c *Config) { c.Graph.GiveUp = 7 },
		},
		{
			name: "a deeply nested leaf keeps its siblings",
			body: "review:\n  ai:\n    mcp:\n      mode: docker\n",
			want: func(c *Config) { c.Review.AI.MCP.Mode = MCPModeDocker },
		},
		{
			name: "several sections at once",
			body: "agent:\n  model: opus\ntest:\n  runner: go\nfanout:\n  enabled: true\n",
			want: func(c *Config) {
				c.Agent.Model = "opus"
				c.Test.Runner = TestRunnerGo
				c.Fanout.Enabled = true
			},
		},
		{
			name: "a false boolean still overrides a true default",
			body: "graph:\n  approval: false\n",
			want: func(c *Config) { c.Graph.Approval = false },
		},
		{
			name: "an empty string still overrides a non-empty default",
			body: "review:\n  sonar:\n    image: \"\"\n",
			want: func(c *Config) { c.Review.Sonar.Image = "" },
		},
		{
			name: "a duration is parsed with ParseDuration semantics",
			body: "graph:\n  node_timeout: 90s\n",
			want: func(c *Config) { c.Graph.NodeTimeout = NewDuration(90_000_000_000) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, ProjectFile, tt.body)
			got, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want := Default()
			tt.want(&want)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("merge changed more than the keys present (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadLayerPrecedence(t *testing.T) {
	xdg := t.TempDir()
	project := t.TempDir()
	explicit := t.TempDir()

	writeFile(t, xdg, UserFile, "agent:\n  model: user-model\ngraph:\n  give_up: 9\nbudget:\n  max_usd: 1.5\n")
	writeFile(t, project, ProjectFile, "agent:\n  model: project-model\ngraph:\n  max_steps: 11\n")
	path := writeFile(t, explicit, "ci.yaml", "agent:\n  model: explicit-model\n")

	got, err := Load(Options{Path: path, Dir: project, XDGConfigHome: xdg})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := Default()
	want.Agent.Model = "explicit-model" // --config beats both files
	want.Graph.GiveUp = 9               // only the user file sets it
	want.Graph.MaxSteps = 11            // only the project file sets it
	want.Budget.MaxUSD = 1.5            // only the user file sets it
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("layer precedence (-want +got):\n%s", diff)
	}
}

func TestLoadUsesXDGConfigHomeFromEnvironment(t *testing.T) {
	xdg := t.TempDir()
	writeFile(t, xdg, UserFile, "agent:\n  model: from-env\n")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := Load(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Agent.Model != "from-env" {
		t.Errorf("agent.model = %q, want %q", got.Agent.Model, "from-env")
	}
}

func TestLoadMissingExplicitPathIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Load(Options{Path: missing, Dir: t.TempDir(), XDGConfigHome: t.TempDir()})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want one wrapping ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the missing file", err)
	}
}

func TestMergeRejectsUnknownField(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{name: "top level typo", body: "budge:\n  max_usd: 3\n", field: "budge"},
		{name: "nested typo", body: "graph:\n  give_upp: 3\n", field: "graph.give_upp"},
		{name: "deeply nested typo", body: "review:\n  sonar:\n    quality_gate_waitt: true\n", field: "review.sonar.quality_gate_waitt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			err := cfg.Merge([]byte(tt.body), "belay.yaml")
			if !errors.Is(err, ErrUnknownField) {
				t.Fatalf("error = %v, want one wrapping ErrUnknownField", err)
			}
			var unknown *UnknownFieldError
			if !errors.As(err, &unknown) {
				t.Fatalf("error %v is not an *UnknownFieldError", err)
			}
			if unknown.Field != tt.field {
				t.Errorf("Field = %q, want %q", unknown.Field, tt.field)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("message %q does not name the field", err)
			}
			if !strings.Contains(err.Error(), "belay.yaml") {
				t.Errorf("message %q does not name the file", err)
			}
			if diff := cmp.Diff(Default(), cfg); diff != "" {
				t.Errorf("a failed merge must not mutate the config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMergeReportsEveryUnknownFieldAtOnce(t *testing.T) {
	cfg := Default()
	err := cfg.Merge([]byte("budge:\n  max_usd: 3\ngraff:\n  give_up: 2\n"), "belay.yaml")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, field := range []string{"budge", "graff"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("message %q does not mention %q", err, field)
		}
	}
}

func TestMergeRejectsSecrets(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
	}{
		{name: "top level token", body: "token: abc123\n", key: "token"},
		{name: "nested sonar token", body: "review:\n  sonar:\n    token: squ_abc\n", key: "token"},
		{name: "api_key", body: "agent:\n  api_key: sk-ant-1\n", key: "api_key"},
		{name: "camelCase apiKey", body: "agent:\n  apiKey: sk-ant-1\n", key: "apiKey"},
		{name: "hyphenated api-key", body: "agent:\n  api-key: sk-ant-1\n", key: "api-key"},
		{name: "screaming env style", body: "SONAR_TOKEN: squ_abc\n", key: "SONAR_TOKEN"},
		{name: "password", body: "review:\n  password: hunter2\n", key: "password"},
		{name: "inside a list", body: "agent:\n  - secret: s\n", key: "secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			err := cfg.Merge([]byte(tt.body), "belay.yaml")
			if !errors.Is(err, ErrSecret) {
				t.Fatalf("error = %v, want one wrapping ErrSecret", err)
			}
			var secret *SecretError
			if !errors.As(err, &secret) {
				t.Fatalf("error %v is not a *SecretError", err)
			}
			if secret.Key != tt.key {
				t.Errorf("Key = %q, want %q", secret.Key, tt.key)
			}
			for _, want := range []string{"environment", "SONAR_TOKEN", "ANTHROPIC_API_KEY"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestMergeSecretBeatsUnknownField(t *testing.T) {
	// A secret key is also an unknown field. The security error must win, so
	// the user is told to move the credential rather than to fix a typo.
	cfg := Default()
	err := cfg.Merge([]byte("sonar_token: squ_abc\n"), "belay.yaml")
	if !errors.Is(err, ErrSecret) {
		t.Fatalf("error = %v, want one wrapping ErrSecret", err)
	}
	if errors.Is(err, ErrUnknownField) {
		t.Errorf("error %v should not be reported as a typo", err)
	}
}

func TestMergeRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "broken syntax", body: "agent:\n\tmodel: x\n", want: "malformed"},
		{name: "wrong scalar type", body: "graph:\n  max_steps: not-a-number\n", want: "graph.max_steps"},
		{name: "mapping where a scalar belongs", body: "agent:\n  model:\n    nested: 1\n", want: "agent.model"},
		{name: "duplicate key", body: "agent:\n  model: a\n  model: b\n", want: "already defined"},
		{name: "second document", body: "agent:\n  model: a\n---\nagent:\n  model: b\n", want: "one YAML document"},
		{name: "duration as a list", body: "graph:\n  node_timeout: [1, 2]\n", want: "duration must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			err := cfg.Merge([]byte(tt.body), "belay.yaml")
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("message %q does not contain %q", err, tt.want)
			}
			if diff := cmp.Diff(Default(), cfg); diff != "" {
				t.Errorf("a failed merge must not mutate the config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ProjectFile, "version: 2\n")
	cfg, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("error = %v, want one wrapping ErrUnsupportedVersion", err)
	}
	var versionErr *VersionError
	if !errors.As(err, &versionErr) || versionErr.Got != 2 {
		t.Fatalf("error %v does not report the version found", err)
	}
	if want := "version: 2 is not supported (want one of: 1)"; !strings.Contains(err.Error(), want) {
		t.Errorf("message %q does not contain %q", err, want)
	}
	if cfg.Version != 2 {
		t.Errorf("Load must return what it read: version = %d, want 2", cfg.Version)
	}
}

func TestIsSecretKey(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"token", true},
		{"TOKEN", true},
		{"sonar_token", true},
		{"SONAR_TOKEN", true},
		{"sonar-token", true},
		{"sonarToken", true},
		{"githubToken", true},
		{"access_token", true},
		{"api_key", true},
		{"apiKey", true},
		{"api-key", true},
		{"ANTHROPIC_API_KEY", true},
		{"anthropic_api_key", true},
		{"secret", true},
		{"client_secret", true},
		{"password", true},
		{"passwd", true},
		{"passphrase", true},
		{"credentials", true},
		{"private_key", true},
		{"privateKey", true},
		{"access_key", true},
		// Legitimate schema keys that a substring rule would reject.
		{"max_tokens", false},
		{"custom_cmd", false},
		{"quality_gate_timeout", false},
		{"node_timeout", false},
		{"on_exceed", false},
		{"isolator", false},
		{"select", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := isSecretKey(tt.key); got != tt.want {
				t.Errorf("isSecretKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestSchemaKeysAreNotSecretShaped walks every key belay itself defines and
// fails if the secret rule would reject a valid configuration file.
func TestSchemaKeysAreNotSecretShaped(t *testing.T) {
	encoded, err := yaml.Marshal(Default())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(encoded, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := scanSecrets(&doc, "belay.yaml"); err != nil {
		t.Fatalf("the default configuration is rejected as containing secrets: %v", err)
	}

	cfg := Default()
	if err := cfg.Merge(encoded, "belay.yaml"); err != nil {
		t.Fatalf("belay cannot read back its own defaults: %v", err)
	}
}

func TestSplitLine(t *testing.T) {
	tests := []struct {
		entry      string
		wantLine   int
		wantDetail string
	}{
		{"line 7: cannot unmarshal", 7, "cannot unmarshal"},
		{"did not find expected key", 0, "did not find expected key"},
		{"line seven: nonsense", 0, "line seven: nonsense"},
		{"line 3", 0, "line 3"},
	}
	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			line, detail := splitLine(tt.entry)
			if line != tt.wantLine || detail != tt.wantDetail {
				t.Errorf("splitLine(%q) = (%d, %q), want (%d, %q)",
					tt.entry, line, detail, tt.wantLine, tt.wantDetail)
			}
		})
	}
}

func TestAtOmitsAnUnknownLine(t *testing.T) {
	if got, want := at("belay.yaml", 0), "belay.yaml"; got != want {
		t.Errorf("at(%q, 0) = %q, want %q", "belay.yaml", got, want)
	}
	if got, want := at("belay.yaml", 4), "belay.yaml:4"; got != want {
		t.Errorf("at(%q, 4) = %q, want %q", "belay.yaml", got, want)
	}
}

func TestDurationRejectsEveryNonScalarShape(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "list", body: "[1, 2]", want: "not a list"},
		{name: "mapping", body: "{a: 1}", want: "not a mapping"},
		{name: "alias resolves to its target", body: "a: &anchor {x: 1}\nb: *anchor", want: "not a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var target any = new(Duration)
			if strings.HasPrefix(tt.name, "alias") {
				target = &struct {
					A map[string]int `yaml:"a"`
					B Duration       `yaml:"b"`
				}{}
			}
			err := yaml.Unmarshal([]byte(tt.body), target)
			if err == nil {
				t.Fatalf("want an error for %s, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("message %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadReportsAnUnreadableFile(t *testing.T) {
	// A directory where a file is expected fails to read for a reason that
	// is not "missing", so it must be reported rather than skipped.
	dir := t.TempDir()
	asDir := filepath.Join(dir, "belay.yaml")
	if err := os.Mkdir(asDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := Load(Options{Dir: dir, XDGConfigHome: t.TempDir()})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("an unreadable file must not be reported as missing: %v", err)
	}
	if !strings.Contains(err.Error(), asDir) {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestConfigHomeFallsBackToTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, filepath.Join(".config", UserFile), "agent:\n  model: from-home\n")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	got, err := Load(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Agent.Model != "from-home" {
		t.Errorf("agent.model = %q, want %q", got.Agent.Model, "from-home")
	}
}
