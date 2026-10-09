//go:build unix

package linter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// diag is one ruff diagnostic, as a test spells it. File is relative to
// "/repo", unless Path names the absolute filename ruff printed instead. A
// zero Row means ruff reported no location at all.
type diag struct {
	Code    string
	File    string
	Path    string
	Row     int
	Message string
}

// ruffOutput renders diagnostics in ruff's json output shape, with ruff's
// hardcoded "error" severity, under the directory "/repo".
func ruffOutput(t *testing.T, diags ...diag) string {
	t.Helper()
	type location struct {
		Row    int `json:"row"`
		Column int `json:"column"`
	}
	type entry struct {
		Code     *string   `json:"code"`
		Filename string    `json:"filename"`
		Location *location `json:"location"`
		Message  string    `json:"message"`
		Severity string    `json:"severity"`
	}
	out := make([]entry, 0, len(diags))
	for _, d := range diags {
		e := entry{Filename: "/repo/" + d.File, Message: d.Message, Severity: "error"}
		if d.Path != "" {
			e.Filename = d.Path
		}
		if d.Code != "" {
			code := d.Code
			e.Code = &code
		}
		if d.Row > 0 {
			e.Location = &location{Row: d.Row, Column: 1}
		}
		out = append(out, e)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal ruff output: %v", err)
	}
	return string(b)
}

// untracked renders paths the way `git ls-files -z` prints them.
func untracked(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return b.String()
}

// located renders issues as sorted "file:line rule" strings.
func located(issues []belay.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, fmt.Sprintf("%s:%d %s", i.File, i.Line, i.RuleID))
	}
	slices.Sort(out)
	return out
}

// serviceDiff rewrites line 14 of app/service.py and deletes the last two
// lines of app/broken.py, which is how a run breaks a file's syntax without
// adding a line to it.
const serviceDiff = `diff --git a/app/service.py b/app/service.py
index 1111111..2222222 100644
--- a/app/service.py
+++ b/app/service.py
@@ -14 +14 @@ def handler():
-    return requests.get(url)
+    return reqeusts.get(url)
diff --git a/app/broken.py b/app/broken.py
index 3333333..4444444 100644
--- a/app/broken.py
+++ b/app/broken.py
@@ -10,2 +9,0 @@ def tail():
-    pass
-)
`

// TestRuffScopesToChangedLines is the product fix: ruff now judges the run's
// change, the way golangci-lint already does. A finding counts when it sits
// on a line the change added or rewrote, or anywhere in a file the run
// created. Findings committed before the run do not count, because they say
// nothing about whether this change may leave the fix loop.
func TestRuffScopesToChangedLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		diags    []diag
		diff     string
		files    string
		want     []string
		wantGate belay.GateStatus
	}{
		{
			name: "committed findings do not count and changed lines do",
			diags: []diag{
				{Code: "F821", File: "app/service.py", Row: 14, Message: "Undefined name `reqeusts`"},
				{Code: "B006", File: "app/service.py", Row: 8, Message: "Do not use mutable data structures for argument defaults"},
				{Code: "S105", File: "app/config.py", Row: 21, Message: "Possible hardcoded password"},
				{Code: "S101", File: "tests/test_service.py", Row: 5, Message: "Use of `assert` detected"},
			},
			diff:     serviceDiff,
			files:    untracked("tests/test_service.py"),
			want:     []string{"app/service.py:14 F821", "tests/test_service.py:5 S101"},
			wantGate: belay.GateFail,
		},
		{
			name:     "a change with no findings on its lines passes despite committed ones",
			diags:    []diag{{Code: "B006", File: "app/service.py", Row: 8, Message: "mutable default"}},
			diff:     serviceDiff,
			want:     []string{},
			wantGate: belay.GatePass,
		},
		{
			// A deleted closing bracket is reported where the parser gives
			// up, which need not be a line the change touched.
			name:     "a syntax error anywhere in a file the change touched counts",
			diags:    []diag{{Code: "E999", File: "app/broken.py", Row: 3, Message: "SyntaxError: Expected an expression"}},
			diff:     serviceDiff,
			want:     []string{"app/broken.py:3 E999"},
			wantGate: belay.GateFail,
		},
		{
			// What ruff 0.8 and later print: the code is the diagnostic's
			// id, and the message does not say SyntaxError.
			name:     "ruff's own invalid-syntax in a touched file counts",
			diags:    []diag{{Code: "invalid-syntax", File: "app/broken.py", Row: 3, Message: "Expected `)`, found newline"}},
			diff:     serviceDiff,
			want:     []string{"app/broken.py:3 invalid-syntax"},
			wantGate: belay.GateFail,
		},
		{
			// A diagnostic with neither a code nor a name, whose message
			// says it is a SyntaxError.
			name:     "a code-less syntax error in a touched file counts",
			diags:    []diag{{File: "app/broken.py", Row: 3, Message: "SyntaxError: Expected ')', found newline"}},
			diff:     serviceDiff,
			want:     []string{"app/broken.py:3 ruff"},
			wantGate: belay.GateFail,
		},
		{
			// Python repositories commit files that do not parse on
			// purpose, as test data. They are not the run's doing.
			name:     "a syntax error in a file the change did not touch does not count",
			diags:    []diag{{Code: "E999", File: "tests/data/invalid.py", Row: 1, Message: "SyntaxError: invalid syntax"}},
			diff:     serviceDiff,
			want:     []string{},
			wantGate: belay.GatePass,
		},
		{
			name: "a finding with no location counts only in a touched file",
			diags: []diag{
				{Code: "E902", File: "app/broken.py", Message: "unreadable"},
				{Code: "E902", File: "app/config.py", Message: "unreadable"},
			},
			diff:     serviceDiff,
			want:     []string{"app/broken.py:0 E902"},
			wantGate: belay.GateFail,
		},
		{
			// A path belay cannot place inside the linted directory cannot be
			// matched against the change at all. It counts, rather than
			// vanishing from the gate.
			name:     "a finding belay cannot place inside the directory counts",
			diags:    []diag{{Code: "F821", Path: "/elsewhere/x.py", Row: 3, Message: "Undefined name `x`"}},
			diff:     serviceDiff,
			want:     []string{"/elsewhere/x.py:3 F821"},
			wantGate: belay.GateFail,
		},
		{
			name: "a new file the change added to the index counts by its added lines",
			diags: []diag{
				{Code: "F821", File: "app/added.py", Row: 3, Message: "Undefined name `x`"},
			},
			diff: "diff --git a/app/added.py b/app/added.py\nnew file mode 100644\nindex 0000000..5555555\n" +
				"--- /dev/null\n+++ b/app/added.py\n@@ -0,0 +1,4 @@\n+import os\n+\n+print(x)\n+\n",
			want:     []string{"app/added.py:3 F821"},
			wantGate: belay.GateFail,
		},
		{
			name:  "a deleted file is not part of the change's lines",
			diags: []diag{{Code: "F821", File: "app/gone.py", Row: 1, Message: "Undefined name `x`"}},
			diff: "diff --git a/app/gone.py b/app/gone.py\ndeleted file mode 100644\nindex 6666666..0000000\n" +
				"--- a/app/gone.py\n+++ /dev/null\n@@ -1 +0,0 @@\n-print(x)\n",
			want:     []string{},
			wantGate: belay.GatePass,
		},
		{
			// git quotes a path holding bytes outside printable ASCII, with
			// C-style octal escapes.
			name:  "a quoted path is unquoted before it is matched",
			diags: []diag{{Code: "F821", File: "app/café.py", Row: 2, Message: "Undefined name `x`"}},
			diff: "diff --git \"a/app/caf\\303\\251.py\" \"b/app/caf\\303\\251.py\"\nindex 7777777..8888888 100644\n" +
				"--- \"a/app/caf\\303\\251.py\"\n+++ \"b/app/caf\\303\\251.py\"\n@@ -2 +2 @@\n-print(y)\n+print(x)\n",
			want:     []string{"app/café.py:2 F821"},
			wantGate: belay.GateFail,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := findingsRunner(ruffOutput(t, tc.diags...), 1)
			runner.git = map[string]scripted{
				"diff":     {stdout: tc.diff},
				"ls-files": {stdout: tc.files},
			}
			var logs bytes.Buffer
			linter := NewRuff(WithRunner(runner), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))

			got, err := linter.Lint(t.Context(), "/repo")
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if diff := cmp.Diff(tc.want, located(got.Issues)); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
			if got.Gate != tc.wantGate {
				t.Errorf("Gate = %v, want %v (%s)", got.Gate, tc.wantGate, got.Summary)
			}
			if strings.Contains(got.Summary, "not scoped") {
				t.Errorf("Summary = %q; a scoped run must not claim it was unscoped", got.Summary)
			}
			if strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("a scoped run logged a warning:\n%s", logs.String())
			}
		})
	}
}

// TestRuffCountsEverythingGitCannotScope covers every way git can fail to say
// what the change is. Each must count every finding, and say so, rather than
// count none: a gate that hides the run's own findings is worse than one that
// also counts old ones.
func TestRuffCountsEverythingGitCannotScope(t *testing.T) {
	t.Parallel()

	diags := []diag{
		{Code: "F821", File: "app/service.py", Row: 14, Message: "Undefined name `reqeusts`"},
		{Code: "S105", File: "app/config.py", Row: 21, Message: "Possible hardcoded password"},
	}
	all := []string{"app/config.py:21 S105", "app/service.py:14 F821"}

	tests := []struct {
		name string
		git  map[string]scripted
	}{
		{"outside a git repository", map[string]scripted{
			"ls-tree": {exitCode: 128, stderr: "fatal: not a git repository (or any of the parent directories): .git"},
		}},
		{"in a repository with no commit", map[string]scripted{
			"ls-tree": {exitCode: 128, stderr: "fatal: Not a valid object name HEAD"},
		}},
		{"in a directory git ignores", map[string]scripted{"ls-tree": {}}},
		{"where git would ignore a new file", map[string]scripted{"check-ignore": {}}},
		{"when git diff fails", map[string]scripted{
			"diff": {exitCode: 128, stderr: "fatal: bad revision 'HEAD'"},
		}},
		{"when git diff is cut off", map[string]scripted{
			"diff": {stdout: serviceDiff, truncated: true},
		}},
		{"when git ls-files fails", map[string]scripted{
			"diff":     {stdout: serviceDiff},
			"ls-files": {exitCode: 128, stderr: "fatal: index file corrupt"},
		}},
		{"when git ls-files is cut off", map[string]scripted{
			"diff":     {stdout: serviceDiff},
			"ls-files": {stdout: untracked("a.py"), truncated: true},
		}},
		{"when git diff prints something that is not a diff", map[string]scripted{
			"diff": {stdout: "diff --git a/x.py b/x.py\n--- a/x.py\n+++ b/x.py\n@@ -1 +not-a-number @@\n"},
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runner := findingsRunner(ruffOutput(t, diags...), 1)
			runner.git = tc.git
			var logs bytes.Buffer
			linter := NewRuff(WithRunner(runner), WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))

			got, err := linter.Lint(t.Context(), "/repo")
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if diff := cmp.Diff(all, located(got.Issues)); diff != "" {
				t.Errorf("findings mismatch (-want +got):\n%s", diff)
			}
			if !strings.Contains(got.Summary, "not scoped to the run's change") {
				t.Errorf("Summary = %q, want it to say the findings were not scoped", got.Summary)
			}
			if !strings.Contains(logs.String(), `level=WARN msg="belay/linter: cannot scope ruff to the run's change`) {
				t.Errorf("no warning that ruff could not be scoped; logs:\n%s", logs.String())
			}
		})
	}
}

// TestRuffScopeCommands pins the git command lines the scope is read from.
// Every flag overrides a setting a repository or a user could otherwise
// change underneath the parser: colour, an external diff driver, a text
// conversion filter, rename detection, the path prefixes and the context
// width. And ruff itself must run last, after git has said what changed.
func TestRuffScopeCommands(t *testing.T) {
	t.Parallel()

	runner := okRunner("[]")
	linter := NewRuff(WithRunner(runner), WithLogger(quietLogger()))
	if _, err := linter.Lint(t.Context(), "/repo"); err != nil {
		t.Fatalf("Lint: %v", err)
	}

	var got [][]string
	for _, c := range runner.callsTo("git") {
		got = append(got, c.Args)
		if c.Dir != "/repo" {
			t.Errorf("git %v ran in %q, want /repo", c.Args, c.Dir)
		}
	}
	want := [][]string{
		{"ls-tree", "--name-only", "HEAD"},
		{"check-ignore", "--quiet", "--no-index", "--", "belay-scope-check.py"},
		{
			"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames",
			"--src-prefix=a/", "--dst-prefix=b/", "--unified=0", "--relative", "HEAD", "--", ".",
		},
		{"ls-files", "--others", "--exclude-standard", "-z", "--", "."},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("git commands mismatch (-want +got):\n%s", diff)
	}
	if last := runner.lastCall(t); last.Path != "ruff" {
		t.Errorf("last command = %q %v, want ruff after git", last.Path, last.Args)
	}
}

// TestParseChangedLines pins the hunk arithmetic the scope rests on. In a
// unified diff with no context, "+c,d" names the d lines starting at c on the
// new side, a missing ",d" means one line, and d of zero means the hunk only
// removed lines.
func TestParseChangedLines(t *testing.T) {
	t.Parallel()

	diff := "diff --git a/a.py b/a.py\n--- a/a.py\n+++ b/a.py\n" +
		"@@ -3 +3 @@\n-x\n+y\n" +
		"@@ -10,0 +11,3 @@ def f():\n+a\n+b\n+c\n" +
		"@@ -20,4 +23,0 @@\n-d\n-e\n-f\n-g\n" +
		"\\ No newline at end of file\n" +
		"diff --git a/b.py b/b.py\n--- a/b.py\n+++ b/b.py\n@@ -1,2 +1,2 @@\n-p\n-q\n+r\n+s\n"

	set, err := parseChangedLines(diff)
	if err != nil {
		t.Fatalf("parseChangedLines: %v", err)
	}

	tests := []struct {
		file    string
		line    int
		covered bool
	}{
		{"a.py", 3, true},
		{"a.py", 2, false},
		{"a.py", 4, false},
		{"a.py", 11, true},
		{"a.py", 13, true},
		{"a.py", 14, false},
		{"a.py", 23, false},
		{"b.py", 1, true},
		{"b.py", 2, true},
		{"b.py", 3, false},
		{"c.py", 1, false},
	}
	for _, tc := range tests {
		if got := set.covers(tc.file, tc.line); got != tc.covered {
			t.Errorf("covers(%s, %d) = %v, want %v", tc.file, tc.line, got, tc.covered)
		}
	}
	for file, want := range map[string]bool{"a.py": true, "b.py": true, "c.py": false} {
		if got := set.touches(file); got != want {
			t.Errorf("touches(%s) = %v, want %v", file, got, want)
		}
	}

	for _, bad := range []string{
		"--- a/x.py\n+++ b/x.py\n@@ -1 +x @@\n",
		"--- a/x.py\n+++ b/x.py\n@@ garbage @@\n",
		"--- a/x.py\n+++ \"b/unterminated\n@@ -1 +1 @@\n",
		"@@ -1 +1 @@\n",
	} {
		if _, err := parseChangedLines(bad); err == nil {
			t.Errorf("parseChangedLines(%q) = nil error, want one", bad)
		}
	}
}
