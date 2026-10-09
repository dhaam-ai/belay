//go:build fixrate

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/test/fixrate"
)

// fakeHost is a machine the command can be run on in a test: every tool it
// probes answers as scripted, and every probe is recorded, so a test can
// prove a refusal came before anything was asked of the host.
type fakeHost struct {
	goarch string
	env    map[string]string
	answer map[string]string // program -> what its probe prints
	fail   map[string]bool   // program -> its probe fails

	mu     sync.Mutex
	probed []string
}

func healthyHost() *fakeHost {
	return &fakeHost{
		goarch: "amd64",
		env:    map[string]string{"ANTHROPIC_API_KEY": "set"},
		answer: map[string]string{
			"docker":  "Server Version: 28.0.0",
			"python3": fixrate.SWEBenchHarnessVersion,
			"claude":  "2.1.300 (Claude Code)",
			"ruff":    "ruff 0.16.9",
			"git":     "git version 2.50.0",
		},
		fail: map[string]bool{},
	}
}

func (h *fakeHost) environment() environment {
	return environment{
		goarch: h.goarch,
		getenv: func(k string) string { return h.env[k] },
		probe: func(_ context.Context, name string, _ ...string) (string, error) {
			h.mu.Lock()
			h.probed = append(h.probed, name)
			h.mu.Unlock()
			if h.fail[name] {
				return "", errors.New(name + ": executable file not found in $PATH")
			}
			return h.answer[name], nil
		},
		// No test reaches a download: every one is refused before it.
		endpoint: "http://127.0.0.1:1",
	}
}

func (h *fakeHost) probes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.probed...)
}

// writeConfig writes a belay.yaml for the corpus and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "belay-swebench.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const goodConfig = "version: 1\nagent:\n  model: claude-sonnet-5\nbudget:\n  max_usd: 3\n"

// TestRun_Refusals pins every refusal the command makes, and that each comes
// before anything is spent: a refused invocation exits 2, says why, and --
// for the ones that need no tool to decide -- asks nothing of the host at all.
func TestRun_Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     func(cfg, out string) []string
		host     func(h *fakeHost)
		config   string
		want     string
		noProbes bool
	}{
		{
			name:     "no mode",
			args:     func(cfg, out string) []string { return []string{"-out", out, "-config", cfg} },
			want:     "-mode",
			noProbes: true,
		},
		{
			name: "live without the confirm token",
			args: func(cfg, out string) []string {
				return []string{"-mode", "live", "-out", out, "-config", cfg, "-max-usd", "350"}
			},
			want:     fixrate.ConfirmLiveSpend,
			noProbes: true,
		},
		{
			name: "live with the wrong confirm token",
			args: func(cfg, out string) []string {
				return []string{"-mode", "live", "-out", out, "-config", cfg, "-max-usd", "350", "-confirm", "yes"}
			},
			want:     fixrate.ConfirmLiveSpend,
			noProbes: true,
		},
		{
			name: "live without a spend cap",
			args: func(cfg, out string) []string {
				return []string{"-mode", "live", "-out", out, "-config", cfg, "-confirm", fixrate.ConfirmLiveSpend}
			},
			want:     "-max-usd",
			noProbes: true,
		},
		{
			name:     "no corpus directory",
			args:     func(cfg, _ string) []string { return []string{"-mode", "replay", "-config", cfg} },
			want:     "-out",
			noProbes: true,
		},
		{
			name:     "no configuration",
			args:     func(_, out string) []string { return []string{"-mode", "replay", "-out", out} },
			want:     "-config",
			noProbes: true,
		},
		{
			name:     "a configuration belay cannot load",
			args:     liveArgs,
			config:   "version: 1\nagent:\n  not_a_real_field: claude-sonnet-5\n",
			want:     "not_a_real_field",
			noProbes: true,
		},
		{
			name:     "a model the backend cannot price",
			args:     liveArgs,
			config:   "version: 1\nagent:\n  model: claude-sonnet-99\n",
			want:     "claude-sonnet-99",
			noProbes: true,
		},
		{
			name:     "a live run on a host that is not x86_64",
			args:     liveArgs,
			host:     func(h *fakeHost) { h.goarch = "arm64" },
			want:     "x86_64",
			noProbes: true,
		},
		{
			name:     "a live run with no key",
			args:     liveArgs,
			host:     func(h *fakeHost) { delete(h.env, "ANTHROPIC_API_KEY") },
			want:     "ANTHROPIC_API_KEY",
			noProbes: true,
		},
		{
			name: "no docker",
			args: liveArgs,
			host: func(h *fakeHost) { h.fail["docker"] = true },
			want: "Docker",
		},
		{
			name: "the wrong swebench",
			args: liveArgs,
			host: func(h *fakeHost) { h.answer["python3"] = "4.1.0" },
			want: "swebench==" + fixrate.SWEBenchHarnessVersion,
		},
		{
			name: "no claude",
			args: liveArgs,
			host: func(h *fakeHost) { h.fail["claude"] = true },
			want: "claude",
		},
		{
			name: "no ruff",
			args: liveArgs,
			host: func(h *fakeHost) { h.fail["ruff"] = true },
			want: "ruff",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host := healthyHost()
			if tt.host != nil {
				tt.host(host)
			}
			body := goodConfig
			if tt.config != "" {
				body = tt.config
			}
			cfg := writeConfig(t, body)
			out := filepath.Join(t.TempDir(), "corpus")
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args(cfg, out), &stdout, &stderr, host.environment())
			if code != exitRefused {
				t.Fatalf("exit = %d, want %d (refused)\nstderr:\n%s", code, exitRefused, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tt.want)
			}
			if tt.noProbes && len(host.probes()) != 0 {
				t.Errorf("the host was probed (%v) before a refusal that needed nothing from it", host.probes())
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused invocation created the corpus directory (stat: %v)", err)
			}
		})
	}
}

// liveArgs is a complete live invocation.
func liveArgs(cfg, out string) []string {
	return []string{"-mode", "live", "-out", out, "-config", cfg, "-max-usd", "350", "-confirm", fixrate.ConfirmLiveSpend}
}

// TestRun_HealthyHostReachesTheDownload: with every check passed the command
// goes on to fetch the pinned corpus -- here from an endpoint that is not
// there, so it stops at that step, having spent nothing.
func TestRun_HealthyHostReachesTheDownload(t *testing.T) {
	t.Parallel()

	host := healthyHost()
	out := filepath.Join(t.TempDir(), "corpus")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), liveArgs(writeConfig(t, goodConfig), out), &stdout, &stderr, host.environment())
	if code != exitError {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "fetching") {
		t.Errorf("stderr = %q, want it to stop at the download", stderr.String())
	}
	for _, tool := range []string{"docker", "python3", "claude", "ruff", "git"} {
		if !strings.Contains(strings.Join(host.probes(), " "), tool) {
			t.Errorf("the preflight never checked %s (probed %v)", tool, host.probes())
		}
	}
}

// TestRun_ScriptedModeNeedsNoKeyAndNoClaude: the free mode spends nothing and
// starts no agent, so it asks for neither.
func TestRun_ScriptedModeNeedsNoKeyAndNoClaude(t *testing.T) {
	t.Parallel()

	host := healthyHost()
	delete(host.env, "ANTHROPIC_API_KEY")
	host.fail["claude"] = true
	out := filepath.Join(t.TempDir(), "corpus")
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-mode", "replay", "-out", out, "-config", writeConfig(t, goodConfig), "-pilot"},
		&stdout, &stderr, host.environment())
	if code != exitError || !strings.Contains(stderr.String(), "fetching") {
		t.Fatalf("exit = %d, want %d at the download\nstderr:\n%s", code, exitError, stderr.String())
	}
	if strings.Contains(strings.Join(host.probes(), " "), "claude") {
		t.Errorf("scripted mode probed for claude: %v", host.probes())
	}
}

// TestSelection pins the plan the command derives from its flags: the
// pilot is stratified and replicated (the A/A run happens on the pilot only),
// the full run is every task once.
func TestSelection(t *testing.T) {
	t.Parallel()

	var tasks []fixrate.Task
	for _, repo := range []string{"a/x", "b/y"} {
		for i := range 30 {
			tasks = append(tasks, fixrate.Task{ID: strings.ReplaceAll(repo, "/", "__") + "-" + string(rune('a'+i)), Repo: repo})
		}
	}
	pilot, reps, label, err := selection(tasks, flags{pilot: true, pilotSize: 50, seed: 1})
	if err != nil {
		t.Fatalf("selection(pilot) error = %v", err)
	}
	if len(pilot) != 50 || reps != 2 || !strings.Contains(label, "seed 1") || !strings.Contains(label, "A/A") {
		t.Errorf("pilot: %d task(s), %d replicate(s), %q; want 50, 2, a label naming the seed and the A/A replicate", len(pilot), reps, label)
	}
	full, reps, label, err := selection(tasks, flags{})
	if err != nil {
		t.Fatalf("selection(full) error = %v", err)
	}
	if len(full) != 60 || reps != 1 || !strings.Contains(label, "all 60") {
		t.Errorf("full: %d task(s), %d replicate(s), %q; want 60, 1, every task", len(full), reps, label)
	}
}
