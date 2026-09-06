package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/state"
	"github.com/google/go-cmp/cmp"
)

// runsBase is a fixed clock for fixtures, so that ages and orderings in
// these tests never depend on when the suite runs.
var runsBase = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// runsManifest returns a healthy manifest for a run.
func runsManifest(id string, status state.RunStatus, created time.Time) state.Manifest {
	return state.Manifest{
		SchemaVersion: state.ManifestSchemaVersion,
		RunID:         id,
		CreatedAt:     created,
		UpdatedAt:     created.Add(time.Minute),
		Workspace:     "/tmp/workspace",
		Goal:          "goal for " + id,
		Status:        status,
		CurrentNode:   "code",
		Step:          4,
		Budget:        state.Budget{LimitUSD: 5, SpentUSD: 0.42},
	}
}

// runsSeed writes a healthy run directory into workspace.
func runsSeed(t *testing.T, workspace string, m state.Manifest) {
	t.Helper()
	layout, err := state.NewLayout(workspace, m.RunID)
	if err != nil {
		t.Fatalf("NewLayout(%q): %v", m.RunID, err)
	}
	if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", layout.RunDir(), err)
	}
	if err := state.SaveManifest(layout.ManifestPath(), m); err != nil {
		t.Fatalf("SaveManifest(%q): %v", layout.ManifestPath(), err)
	}
}

// runsSeedRaw writes a run directory holding exactly the given manifest
// bytes, or no manifest.json at all when manifest is nil.
//
// It joins ".belay/runs" itself rather than going through state.Layout
// because several of these fixtures are names Layout refuses to build a
// path for, which is the very case being tested.
func runsSeedRaw(t *testing.T, workspace, id string, manifest []byte) string {
	t.Helper()
	dir := filepath.Join(workspace, ".belay", "runs", id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	if manifest != nil {
		path := filepath.Join(dir, "manifest.json")
		if err := os.WriteFile(path, manifest, 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
	return dir
}

// runsSeedRawAt seeds a damaged run and pins its directory mtime.
//
// A run whose manifest cannot be read has no readable CreatedAt, so the
// listing falls back to the directory's mtime -- which, for a freshly seeded
// directory, is now. Any test asserting a fixed order among damaged and
// healthy runs must therefore set it, or the order depends on the wall clock
// rather than on the fixture.
func runsSeedRawAt(t *testing.T, workspace, id string, manifest []byte, mtime time.Time) string {
	t.Helper()
	dir := runsSeedRaw(t, workspace, id, manifest)
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("Chtimes(%q): %v", dir, err)
	}
	return dir
}

// runsIDs extracts the run ids of summaries, in order.
func runsIDs(summaries []RunSummary) []string {
	ids := make([]string, len(summaries))
	for i, s := range summaries {
		ids[i] = s.RunID
	}
	return ids
}

func TestRunsScanWithoutRunsDirectory(t *testing.T) {
	t.Parallel()

	got, err := ScanRuns(t.TempDir())
	if err != nil {
		t.Fatalf("ScanRuns: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ScanRuns = %d summaries, want 0", len(got))
	}
	if got == nil {
		t.Error("ScanRuns returned a nil slice; want an empty one so --json emits [] rather than null")
	}
}

func TestRunsScanRejectsRelativeWorkspace(t *testing.T) {
	t.Parallel()

	if _, err := ScanRuns("relative/path"); err == nil {
		t.Fatal("ScanRuns(relative) = nil error, want one: a relative workspace silently resolves against the wrong directory")
	}
}

func TestRunsScanSortsNewestFirst(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	runsSeed(t, workspace, runsManifest("run-oldest", state.RunStatusCompleted, runsBase.Add(-48*time.Hour)))
	runsSeed(t, workspace, runsManifest("run-newest", state.RunStatusRunning, runsBase))
	runsSeed(t, workspace, runsManifest("run-middle", state.RunStatusPaused, runsBase.Add(-2*time.Hour)))

	got, err := ScanRuns(workspace)
	if err != nil {
		t.Fatalf("ScanRuns: %v", err)
	}
	want := []string{"run-newest", "run-middle", "run-oldest"}
	if diff := cmp.Diff(want, runsIDs(got)); diff != "" {
		t.Errorf("ScanRuns order mismatch (-want +got):\n%s", diff)
	}
}

func TestRunsScanTiebreaksDeterministically(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	for _, id := range []string{"run-b", "run-c", "run-a"} {
		runsSeed(t, workspace, runsManifest(id, state.RunStatusCompleted, runsBase))
	}

	// Same CreatedAt for all three: the id tiebreak must still give one
	// stable order, or the listing reshuffles between invocations.
	for range 5 {
		got, err := ScanRuns(workspace)
		if err != nil {
			t.Fatalf("ScanRuns: %v", err)
		}
		want := []string{"run-c", "run-b", "run-a"}
		if diff := cmp.Diff(want, runsIDs(got)); diff != "" {
			t.Fatalf("ScanRuns order mismatch (-want +got):\n%s", diff)
		}
	}
}

func TestRunsScanListsDamagedRunsAsUnreadable(t *testing.T) {
	t.Parallel()

	valid, err := os.ReadFile(runsValidManifestFixture(t))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	tests := []struct {
		name       string
		id         string
		manifest   []byte
		wantReason string
	}{
		{
			name:       "no manifest at all",
			id:         "run-no-manifest",
			manifest:   nil,
			wantReason: "no manifest.json",
		},
		{
			name:       "truncated manifest",
			id:         "run-truncated",
			manifest:   valid[:len(valid)/2],
			wantReason: "manifest.json is damaged",
		},
		{
			name:       "empty manifest",
			id:         "run-empty",
			manifest:   []byte{},
			wantReason: "manifest.json is damaged",
		},
		{
			name:       "not json at all",
			id:         "run-garbage",
			manifest:   []byte("this is not json\x00\x01"),
			wantReason: "manifest.json is damaged",
		},
		{
			name:       "unknown schema version",
			id:         "run-future",
			manifest:   []byte(`{"schema_version": 99, "run_id": "run-future"}`),
			wantReason: "unsupported manifest version 99",
		},
		{
			name:       "directory name is not a usable run id",
			id:         "run..escape",
			manifest:   valid,
			wantReason: "not a usable run id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspace := t.TempDir()
			dir := runsSeedRaw(t, workspace, tt.id, tt.manifest)
			// A healthy run beside the damaged one: a listing must not
			// lose the good runs because one directory is broken.
			runsSeed(t, workspace, runsManifest("run-healthy", state.RunStatusRunning, runsBase))

			got, err := ScanRuns(workspace)
			if err != nil {
				t.Fatalf("ScanRuns: unexpected error, a damaged run must not fail the listing: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("ScanRuns = %d summaries (%v), want 2: the damaged run must be listed, not omitted", len(got), runsIDs(got))
			}

			var damaged *RunSummary
			for i := range got {
				if got[i].RunID == tt.id {
					damaged = &got[i]
				}
			}
			if damaged == nil {
				t.Fatalf("ScanRuns omitted the damaged run %q; got %v", tt.id, runsIDs(got))
			}
			if !damaged.Unreadable {
				t.Error("Unreadable = false, want true")
			}
			if damaged.Status != runsStatusUnreadable {
				t.Errorf("Status = %q, want %q", damaged.Status, runsStatusUnreadable)
			}
			if damaged.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", damaged.Reason, tt.wantReason)
			}
			if damaged.Path != dir {
				t.Errorf("Path = %q, want %q", damaged.Path, dir)
			}
			if damaged.CreatedAt.IsZero() {
				t.Error("CreatedAt is zero; want the directory mtime so a damaged run still sorts sensibly")
			}
			if loc := damaged.CreatedAt.Location(); loc != time.UTC {
				t.Errorf("CreatedAt is in %v, want UTC so one listing does not mix time offsets", loc)
			}
			if !damaged.UpdatedAt.Equal(damaged.CreatedAt) {
				t.Errorf("UpdatedAt = %v, want the directory mtime %v rather than the zero time",
					damaged.UpdatedAt, damaged.CreatedAt)
			}
		})
	}
}

// runsValidManifestFixture writes one healthy manifest and returns its path,
// so damaged fixtures can be built by mutilating real output rather than a
// hand-written guess at the format.
func runsValidManifestFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	m := runsManifest("run-fixture", state.RunStatusCompleted, runsBase)
	runsSeed(t, workspace, m)
	layout, err := state.NewLayout(workspace, m.RunID)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return layout.ManifestPath()
}

func TestRunsScanSkipsNonDirectories(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	runsSeed(t, workspace, runsManifest("run-real", state.RunStatusRunning, runsBase))

	stray := filepath.Join(workspace, ".belay", "runs", ".DS_Store")
	if err := os.WriteFile(stray, []byte("junk"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ScanRuns(workspace)
	if err != nil {
		t.Fatalf("ScanRuns: %v", err)
	}
	if diff := cmp.Diff([]string{"run-real"}, runsIDs(got)); diff != "" {
		t.Errorf("ScanRuns mismatch (-want +got):\n%s", diff)
	}
}

func TestRunsScanHumanizesStatusAndLocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        state.RunStatus
		node          string
		wantStatus    string
		wantManifest  string
		wantWhere     string
		wantResumable bool
	}{
		{
			name: "running", status: state.RunStatusRunning, node: "code",
			wantStatus: "running", wantManifest: "running", wantWhere: "code", wantResumable: true,
		},
		{
			name:   "paused at approve says what it is waiting for",
			status: state.RunStatusPaused, node: "approve",
			wantStatus: "paused", wantManifest: "paused",
			wantWhere: "needs plan approval", wantResumable: true,
		},
		{
			name: "paused elsewhere", status: state.RunStatusPaused, node: "test",
			wantStatus: "paused", wantManifest: "paused", wantWhere: "waiting at test", wantResumable: true,
		},
		{
			name: "completed reads as finished", status: state.RunStatusCompleted, node: "review",
			wantStatus: "finished", wantManifest: "completed", wantWhere: "review",
		},
		{
			name: "failed", status: state.RunStatusFailed, node: "fix",
			wantStatus: "failed", wantManifest: "failed", wantWhere: "fix",
		},
		{
			name: "aborted reads as stopped", status: state.RunStatusAborted, node: "code",
			wantStatus: "stopped", wantManifest: "aborted", wantWhere: "code",
		},
		{
			name: "unknown", status: state.RunStatusUnknown, node: "",
			wantStatus: "unknown", wantManifest: "unknown", wantWhere: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspace := t.TempDir()
			m := runsManifest("run-x", tt.status, runsBase)
			m.CurrentNode = tt.node
			runsSeed(t, workspace, m)

			got, err := ScanRuns(workspace)
			if err != nil {
				t.Fatalf("ScanRuns: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("ScanRuns = %d summaries, want 1", len(got))
			}
			s := got[0]
			if s.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", s.Status, tt.wantStatus)
			}
			if s.ManifestStatus != tt.wantManifest {
				t.Errorf("ManifestStatus = %q, want %q", s.ManifestStatus, tt.wantManifest)
			}
			if s.Where != tt.wantWhere {
				t.Errorf("Where = %q, want %q", s.Where, tt.wantWhere)
			}
			if s.CurrentNode != tt.node {
				t.Errorf("CurrentNode = %q, want %q", s.CurrentNode, tt.node)
			}
			if s.Resumable != tt.wantResumable {
				t.Errorf("Resumable = %t, want %t", s.Resumable, tt.wantResumable)
			}
		})
	}
}

func TestRunsScanReportsIDFromDirectoryName(t *testing.T) {
	t.Parallel()

	// The directory name is what a user types to resume the run, so it
	// wins over a manifest that disagrees with it.
	workspace := t.TempDir()
	m := runsManifest("run-on-disk", state.RunStatusPaused, runsBase)
	runsSeed(t, workspace, m)

	layout, err := state.NewLayout(workspace, "run-on-disk")
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	m.RunID = "a-different-id"
	if err := state.SaveManifest(layout.ManifestPath(), m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}

	got, err := ScanRuns(workspace)
	if err != nil {
		t.Fatalf("ScanRuns: %v", err)
	}
	if got[0].RunID != "run-on-disk" {
		t.Errorf("RunID = %q, want %q (the directory name is what the user must type)", got[0].RunID, "run-on-disk")
	}
}

func TestRunsSanitizeText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text is untouched", in: "add a wc command", want: "add a wc command"},
		{name: "newlines collapse to spaces", in: "line one\nline two", want: "line one line two"},
		{name: "tabs and runs of spaces collapse", in: "a\t\t  b", want: "a b"},
		{name: "leading and trailing whitespace goes", in: "  hi  ", want: "hi"},
		{name: "ansi escape sequence is removed", in: "goal\x1b[31mred\x1b[0m", want: "goal[31mred[0m"},
		{name: "bare escape is removed", in: "a\x1bb", want: "ab"},
		{name: "carriage return is removed", in: "overwrite\rme", want: "overwrite me"},
		{name: "nul byte is removed", in: "a\x00b", want: "ab"},
		{name: "unicode is preserved", in: "café ☕", want: "café ☕"},
		{name: "empty stays empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sanitizeRunsText(tt.in); got != tt.want {
				t.Errorf("sanitizeRunsText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// runsRenderFixture is a listing in the states a person actually meets:
// one paused and waiting on them, one still running on an estimated cost,
// one finished, one failed, and one whose directory belay cannot read.
func runsRenderFixture() []RunSummary {
	return []RunSummary{
		{
			RunID: "20260905T114600Z-a1b2c3d4", Status: runsStatusPaused, ManifestStatus: "paused",
			Where: "needs plan approval", CurrentNode: "approve", Step: 3,
			CreatedAt: runsBase.Add(-14 * time.Minute), UpdatedAt: runsBase.Add(-2 * time.Minute),
			Goal: "add a wc command with tests", Resumable: true,
			Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5},
		},
		{
			RunID: "20260905T090000Z-99887766", Status: runsStatusRunning, ManifestStatus: "running",
			Where: "code", CurrentNode: "code", Step: 12,
			CreatedAt: runsBase.Add(-3 * time.Hour), Resumable: true,
			Goal: "port the entire billing subsystem to the new ledger API and backfill",
			Cost: RunCost{SpentUSD: 12.3, LimitUSD: 20, Estimated: true},
		},
		{
			RunID: "20260904T101500Z-deadbeef", Status: runsStatusFinished, ManifestStatus: "completed",
			Where: "review", CurrentNode: "review", Step: 21,
			CreatedAt: runsBase.Add(-26 * time.Hour),
			Goal:      "fix the off-by-one in wc",
			Cost:      RunCost{SpentUSD: 1.05, LimitUSD: 5},
		},
		{
			RunID: "20260903T083000Z-0badcafe", Status: runsStatusFailed, ManifestStatus: "failed",
			Where: "fix", CurrentNode: "fix", Step: 9,
			CreatedAt: runsBase.Add(-50 * time.Hour),
			Goal:      "make the flaky integration test deterministic",
			Cost:      RunCost{SpentUSD: 3.7, LimitUSD: 5},
		},
		{
			RunID: "20260902T120000Z-11223344", Unreadable: true, Reason: "no manifest.json",
			Status: runsStatusUnreadable, CreatedAt: runsBase.Add(-72 * time.Hour),
		},
	}
}

// runsGoldenTable is the listing exactly as a person sees it. It is here in
// full, rather than asserted property by property, because the alignment of
// these columns against each other is the feature.
const runsGoldenTable = `
RUN ID                     STATUS      WHERE                STEPS         STARTED  COST               GOAL
20260905T114600Z-a1b2c3d4  paused      needs plan approval      3  14 minutes ago    $0.42 of $5.00   add a wc command with tests
20260905T090000Z-99887766  running     code                    12     3 hours ago  ~$12.30 of $20.00  port the entire billing subsystem t…
20260904T101500Z-deadbeef  finished    review                  21       1 day ago    $1.05 of $5.00   fix the off-by-one in wc
20260903T083000Z-0badcafe  failed      fix                      9      2 days ago    $3.70 of $5.00   make the flaky integration test det…
20260902T120000Z-11223344  unreadable  —                        —      3 days ago        —            no manifest.json

7 more runs not shown. Pass --limit 12 to see them all, or --limit 0 for no cap.
~ marks a cost that includes estimated usage rather than reported figures.
Resume the paused run:  belay resume 20260905T114600Z-a1b2c3d4
`

// runsANSI matches the SGR sequences emph emits.
var runsANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// runsRender renders the fixture and returns what was written.
func runsRender(rows []RunSummary, total int, color bool) string {
	var buf bytes.Buffer
	u := &ui{out: &buf, err: &buf, color: color}
	renderRunsTable(u, rows, total, runsBase)
	return buf.String()
}

func TestRunsRenderTable(t *testing.T) {
	t.Parallel()

	rows := runsRenderFixture()
	got := "\n" + runsRender(rows, len(rows)+7, false)
	if diff := cmp.Diff(runsGoldenTable, got); diff != "" {
		t.Errorf("rendered table mismatch (-want +got):\n%s", diff)
	}
}

func TestRunsRenderHasNoEscapeSequencesWithoutATerminal(t *testing.T) {
	t.Parallel()

	// This is the `belay runs > runs.txt` case: a bytes.Buffer is not a
	// character device, so colorEnabled says no and the output must be
	// clean text -- no escapes, and no trailing whitespace either.
	rows := runsRenderFixture()
	got := runsRender(rows, len(rows)+7, false)

	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("output contains an escape sequence:\n%q", got)
	}
	for i, line := range strings.Split(got, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i+1, line)
		}
	}
}

func TestRunsRenderEmphasisDoesNotSkewColumns(t *testing.T) {
	t.Parallel()

	// The whole reason this file pads by hand instead of using
	// text/tabwriter: escape sequences must not count toward a column's
	// width. Strip them back out of a coloured render and what is left
	// has to be the plain table, byte for byte.
	rows := runsRenderFixture()
	colored := runsRender(rows, len(rows)+7, true)

	if !strings.Contains(colored, "\x1b[1m") {
		t.Fatal("coloured render carries no emphasis at all")
	}
	if diff := cmp.Diff(runsRender(rows, len(rows)+7, false), runsANSI.ReplaceAllString(colored, "")); diff != "" {
		t.Errorf("stripping emphasis did not reproduce the plain table (-plain +stripped):\n%s", diff)
	}
}

func TestRunsRenderEmphasisesStatusAndCostOnly(t *testing.T) {
	t.Parallel()

	// Emphasis is a budget. It is spent on the two things people scan a
	// run list for -- how it ended and what it cost -- and nowhere else.
	rows := []RunSummary{{
		RunID: "run-1", Status: runsStatusPaused, Where: "needs plan approval",
		Step: 3, CreatedAt: runsBase, Goal: "a goal",
		Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5},
	}}
	colored := runsRender(rows, 1, true)

	for _, want := range []string{"\x1b[1mpaused\x1b[0m", "\x1b[1m$0.42 of $5.00\x1b[0m"} {
		if !strings.Contains(colored, want) {
			t.Errorf("emphasis missing for %q in:\n%q", want, colored)
		}
	}
	for _, plain := range []string{"run-1", "needs plan approval", "a goal"} {
		if strings.Contains(colored, "\x1b[1m"+plain) {
			t.Errorf("%q is emphasised but should be plain", plain)
		}
	}
}

func TestRunsFooterSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	t.Parallel()

	rows := []RunSummary{{
		RunID: "run-1", Status: runsStatusFinished, Where: "review",
		Step: 3, CreatedAt: runsBase, Goal: "a goal",
		Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5},
	}}
	got := runsRender(rows, len(rows), false)

	if lines := strings.Count(strings.TrimRight(got, "\n"), "\n") + 1; lines != 2 {
		t.Errorf("got %d lines, want 2 (a header and one run, with no notes):\n%s", lines, got)
	}
	for _, unwanted := range []string{"not shown", "~ marks", "belay resume"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unwanted note %q in:\n%s", unwanted, got)
		}
	}
}

func TestRunsFooterPointsAtTheNewestPausedRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		paused []string
		want   string
	}{
		{
			name:   "one paused run names the command outright",
			paused: []string{"run-a"},
			want:   "Resume the paused run:  belay resume run-a",
		},
		{
			name:   "several paused runs point at the newest",
			paused: []string{"run-a", "run-b", "run-c"},
			want:   "3 runs are paused. Resume the newest:  belay resume run-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows := make([]RunSummary, 0, len(tt.paused))
			for i, id := range tt.paused {
				rows = append(rows, RunSummary{
					RunID: id, Status: runsStatusPaused, Where: "needs plan approval",
					CreatedAt: runsBase.Add(-time.Duration(i) * time.Hour), Goal: "a goal",
				})
			}
			got := runsRender(rows, len(rows), false)
			if !strings.Contains(got, tt.want) {
				t.Errorf("want footer %q in:\n%s", tt.want, got)
			}
		})
	}
}

func TestRunsAge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		then time.Time
		want string
	}{
		{name: "unknown start time", then: time.Time{}, want: "—"},
		{name: "same instant", then: runsBase, want: "just now"},
		{name: "clock skew into the future", then: runsBase.Add(time.Hour), want: "just now"},
		{name: "under a minute", then: runsBase.Add(-30 * time.Second), want: "just now"},
		{name: "one minute", then: runsBase.Add(-time.Minute), want: "1 minute ago"},
		{name: "minutes", then: runsBase.Add(-14 * time.Minute), want: "14 minutes ago"},
		{name: "one hour", then: runsBase.Add(-time.Hour), want: "1 hour ago"},
		{name: "hours", then: runsBase.Add(-3 * time.Hour), want: "3 hours ago"},
		{name: "one day", then: runsBase.Add(-26 * time.Hour), want: "1 day ago"},
		{name: "days", then: runsBase.Add(-72 * time.Hour), want: "3 days ago"},
		{name: "one week", then: runsBase.AddDate(0, 0, -8), want: "1 week ago"},
		{name: "weeks", then: runsBase.AddDate(0, 0, -21), want: "3 weeks ago"},
		{name: "months", then: runsBase.AddDate(0, 0, -70), want: "2 months ago"},
		{name: "years", then: runsBase.AddDate(-2, 0, 0), want: "2 years ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runsAge(runsBase, tt.then); got != tt.want {
				t.Errorf("runsAge(%v) = %q, want %q", tt.then, got, tt.want)
			}
		})
	}
}

func TestRunsCostCell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		row  RunSummary
		want string
	}{
		{
			name: "spend against a ceiling",
			row:  RunSummary{Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5}},
			want: " $0.42 of $5.00",
		},
		{
			name: "an estimate is marked as one",
			row:  RunSummary{Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5, Estimated: true}},
			want: "~$0.42 of $5.00",
		},
		{
			name: "no ceiling says so rather than claiming $0.00",
			row:  RunSummary{Cost: RunCost{SpentUSD: 0.42}},
			want: " $0.42 (no limit)",
		},
		{
			name: "nothing spent yet",
			row:  RunSummary{Cost: RunCost{LimitUSD: 5}},
			want: " $0.00 of $5.00",
		},
		{
			name: "over budget is shown, not hidden",
			row:  RunSummary{Cost: RunCost{SpentUSD: 7.5, LimitUSD: 5}},
			want: " $7.50 of $5.00",
		},
		{
			name: "an unreadable run has no cost to report",
			row:  RunSummary{Unreadable: true},
			want: "     —",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Width 6 is what "~$0.42" needs, so every want above shows
			// the padding a real column would apply.
			if got := runsCostCell(tt.row, 6); got != tt.want {
				t.Errorf("runsCostCell = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunsTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "short enough is untouched", in: "abc", max: 5, want: "abc"},
		{name: "exactly at the cap is untouched", in: "abcde", max: 5, want: "abcde"},
		{name: "one over the cap is cut", in: "abcdef", max: 5, want: "abcd…"},
		{name: "counts runes not bytes", in: "ααααα", max: 5, want: "ααααα"},
		{name: "cuts on runes not bytes", in: "αααααα", max: 5, want: "αααα…"},
		{name: "empty stays empty", in: "", max: 5, want: ""},
		{name: "a zero cap disables truncation", in: "abcdef", max: 0, want: "abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := runsTruncate(tt.in, tt.max)
			if got != tt.want {
				t.Errorf("runsTruncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if tt.max > 0 && runsRuneLen(got) > tt.max {
				t.Errorf("runsTruncate(%q, %d) = %q, which is %d runes", tt.in, tt.max, got, runsRuneLen(got))
			}
		})
	}
}

// runsGoldenJSON is the documented shape of "belay runs --json", shown
// with one healthy run and one belay could not read. Every key is always
// present, so a script never has to tell a missing key from an empty one.
const runsGoldenJSON = `{
  "workspace": "/home/dev/project",
  "total": 9,
  "shown": 2,
  "hidden": 7,
  "runs": [
    {
      "run_id": "20260905T114600Z-a1b2c3d4",
      "unreadable": false,
      "reason": "",
      "status": "paused",
      "manifest_status": "paused",
      "where": "needs plan approval",
      "current_node": "approve",
      "step": 3,
      "created_at": "2026-09-05T11:46:00Z",
      "updated_at": "2026-09-05T11:58:00Z",
      "goal": "add a wc command with tests",
      "resumable": true,
      "cost": {
        "spent_usd": 0.42,
        "limit_usd": 5,
        "estimated": false,
        "tokens_in": 18412,
        "tokens_out": 3907
      },
      "path": "/home/dev/project/.belay/runs/20260905T114600Z-a1b2c3d4"
    },
    {
      "run_id": "20260902T120000Z-11223344",
      "unreadable": true,
      "reason": "no manifest.json",
      "status": "unreadable",
      "manifest_status": "",
      "where": "",
      "current_node": "",
      "step": 0,
      "created_at": "2026-09-02T12:00:00Z",
      "updated_at": "2026-09-02T12:00:00Z",
      "goal": "",
      "resumable": false,
      "cost": {
        "spent_usd": 0,
        "limit_usd": 0,
        "estimated": false,
        "tokens_in": 0,
        "tokens_out": 0
      },
      "path": "/home/dev/project/.belay/runs/20260902T120000Z-11223344"
    }
  ]
}
`

// runsJSONFixture is the listing runsGoldenJSON describes.
func runsJSONFixture() RunsListing {
	return RunsListing{
		Workspace: "/home/dev/project", Total: 9, Shown: 2, Hidden: 7,
		Runs: []RunSummary{
			{
				RunID: "20260905T114600Z-a1b2c3d4", Status: runsStatusPaused, ManifestStatus: "paused",
				Where: "needs plan approval", CurrentNode: "approve", Step: 3,
				CreatedAt: runsBase.Add(-14 * time.Minute), UpdatedAt: runsBase.Add(-2 * time.Minute),
				Goal: "add a wc command with tests", Resumable: true,
				Cost: RunCost{SpentUSD: 0.42, LimitUSD: 5, TokensIn: 18412, TokensOut: 3907},
				Path: "/home/dev/project/.belay/runs/20260905T114600Z-a1b2c3d4",
			},
			{
				RunID: "20260902T120000Z-11223344", Unreadable: true, Reason: "no manifest.json",
				Status: runsStatusUnreadable, CreatedAt: runsBase.Add(-72 * time.Hour),
				UpdatedAt: runsBase.Add(-72 * time.Hour),
				Path:      "/home/dev/project/.belay/runs/20260902T120000Z-11223344",
			},
		},
	}
}

func TestRunsJSONShapeIsStable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := writeRunsJSON(&buf, runsJSONFixture()); err != nil {
		t.Fatalf("writeRunsJSON: %v", err)
	}
	if diff := cmp.Diff(runsGoldenJSON, buf.String()); diff != "" {
		t.Errorf("JSON shape changed (-want +got):\n%s\n\nThis shape is a published contract: "+
			"renaming or dropping a key breaks every script reading it.", diff)
	}
}

func TestRunsJSONRunsIsNeverNull(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := writeRunsJSON(&buf, RunsListing{Workspace: "/w"}); err != nil {
		t.Fatalf("writeRunsJSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"runs": []`) {
		t.Errorf("empty listing should carry an empty array, got:\n%s", buf.String())
	}
	var round RunsListing
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

// runsRun drives the command's core against a workspace and returns stdout.
func runsRun(t *testing.T, opts runsOptions) string {
	t.Helper()
	var buf bytes.Buffer
	if opts.now.IsZero() {
		opts.now = runsBase
	}
	if err := runRuns(&ui{out: &buf, err: &buf}, opts); err != nil {
		t.Fatalf("runRuns: %v", err)
	}
	return buf.String()
}

func TestRunsCommandSaysSoWhenThereAreNoRuns(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	got := runsRun(t, runsOptions{workspace: workspace, limit: runsDefaultLimit})

	if !strings.Contains(got, "No runs yet") {
		t.Errorf("want a plain sentence about there being no runs, got:\n%s", got)
	}
	if !strings.Contains(got, workspace) {
		t.Errorf("the sentence should say where belay looked, got:\n%s", got)
	}
	if strings.Contains(got, "RUN ID") {
		t.Errorf("an empty workspace should not print a table header, got:\n%s", got)
	}
}

func TestRunsCommandEmitsJSONEvenWithNoRuns(t *testing.T) {
	t.Parallel()

	// A script asked for JSON. It must not have to parse an English
	// sentence to discover the workspace is empty.
	got := runsRun(t, runsOptions{workspace: t.TempDir(), limit: runsDefaultLimit, asJSON: true})

	var listing RunsListing
	if err := json.Unmarshal([]byte(got), &listing); err != nil {
		t.Fatalf("--json produced non-JSON output %q: %v", got, err)
	}
	if listing.Total != 0 || listing.Shown != 0 || listing.Hidden != 0 {
		t.Errorf("counts = total %d/shown %d/hidden %d, want all zero", listing.Total, listing.Shown, listing.Hidden)
	}
	if listing.Runs == nil {
		t.Error("runs is null, want an empty array")
	}
}

func TestRunsCommandJSONReportsRealRuns(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	runsSeed(t, workspace, runsManifest("run-new", state.RunStatusPaused, runsBase))
	runsSeed(t, workspace, runsManifest("run-old", state.RunStatusCompleted, runsBase.Add(-time.Hour)))
	// Pinned older than run-old: an unreadable run is ordered by directory
	// mtime, so leaving it at "now" would sort it above a run the fixture
	// says is an hour old and make this assertion depend on the clock.
	runsSeedRawAt(t, workspace, "run-broken", []byte("{"), runsBase.Add(-2*time.Hour))

	got := runsRun(t, runsOptions{workspace: workspace, limit: runsDefaultLimit, asJSON: true})

	var listing RunsListing
	if err := json.Unmarshal([]byte(got), &listing); err != nil {
		t.Fatalf("--json produced non-JSON output: %v", err)
	}
	if listing.Workspace != workspace {
		t.Errorf("workspace = %q, want %q", listing.Workspace, workspace)
	}
	if listing.Total != 3 || listing.Shown != 3 || listing.Hidden != 0 {
		t.Errorf("counts = total %d/shown %d/hidden %d, want 3/3/0", listing.Total, listing.Shown, listing.Hidden)
	}
	if diff := cmp.Diff([]string{"run-new", "run-old", "run-broken"}, runsIDs(listing.Runs)); diff != "" {
		t.Errorf("runs mismatch (-want +got):\n%s", diff)
	}
	if !listing.Runs[2].Unreadable {
		t.Error("the damaged run should be marked unreadable in JSON, not omitted")
	}
	if !strings.HasPrefix(listing.Runs[0].Path, workspace) {
		t.Errorf("path %q should be inside the workspace %q", listing.Runs[0].Path, workspace)
	}
}

func TestRunsCommandLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		limit      int
		wantShown  int
		wantHidden int
		wantNote   string
	}{
		{name: "under the cap says nothing about it", limit: 20, wantShown: 5, wantHidden: 0},
		{name: "the cap says how many it held back", limit: 2, wantShown: 2, wantHidden: 3,
			wantNote: "3 more runs not shown. Pass --limit 5 to see them all, or --limit 0 for no cap."},
		{name: "a cap of one is still counted correctly", limit: 4, wantShown: 4, wantHidden: 1,
			wantNote: "1 more run not shown. Pass --limit 5 to see them all, or --limit 0 for no cap."},
		{name: "zero means no cap", limit: 0, wantShown: 5, wantHidden: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workspace := t.TempDir()
			for i := range 5 {
				id := fmt.Sprintf("run-%d", i)
				runsSeed(t, workspace, runsManifest(id, state.RunStatusCompleted, runsBase.Add(-time.Duration(i)*time.Hour)))
			}

			table := runsRun(t, runsOptions{workspace: workspace, limit: tt.limit})
			// The header is one line; the rest are runs, notes and the
			// blank line that separates them.
			rows := 0
			for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
				if strings.HasPrefix(line, "run-") {
					rows++
				}
			}
			if rows != tt.wantShown {
				t.Errorf("table shows %d runs, want %d:\n%s", rows, tt.wantShown, table)
			}
			if tt.wantNote == "" {
				if strings.Contains(table, "not shown") {
					t.Errorf("nothing was hidden, so nothing should be said about it:\n%s", table)
				}
			} else if !strings.Contains(table, tt.wantNote) {
				t.Errorf("want note %q in:\n%s", tt.wantNote, table)
			}

			jsonOut := runsRun(t, runsOptions{workspace: workspace, limit: tt.limit, asJSON: true})
			var listing RunsListing
			if err := json.Unmarshal([]byte(jsonOut), &listing); err != nil {
				t.Fatalf("--json: %v", err)
			}
			// Truncation must be as visible to a script as to a person.
			if listing.Shown != tt.wantShown || listing.Hidden != tt.wantHidden {
				t.Errorf("JSON shown/hidden = %d/%d, want %d/%d",
					listing.Shown, listing.Hidden, tt.wantShown, tt.wantHidden)
			}
		})
	}
}

func TestRunsCommandRejectsNegativeLimit(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := runRuns(&ui{out: &buf, err: &buf}, runsOptions{workspace: t.TempDir(), limit: -1, now: runsBase})
	if err == nil {
		t.Fatal("runRuns(--limit -1) = nil error, want one")
	}
	if !strings.Contains(err.Error(), "--limit") {
		t.Errorf("error %q should name the flag the user got wrong", err)
	}
}

func TestRunsCommandListsDamagedRunsWithoutFailing(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	runsSeed(t, workspace, runsManifest("run-good", state.RunStatusRunning, runsBase))
	runsSeedRaw(t, workspace, "run-truncated", []byte(`{"schema_version": 1, "run_i`))
	runsSeedRaw(t, workspace, "run-nomanifest", nil)

	got := runsRun(t, runsOptions{workspace: workspace, limit: runsDefaultLimit})

	for _, want := range []string{
		"run-good", "run-truncated", "run-nomanifest",
		"unreadable", "manifest.json is damaged", "no manifest.json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in the listing:\n%s", want, got)
		}
	}
}

func TestRunsCommandIsRegisteredOnTheRoot(t *testing.T) {
	t.Parallel()

	var found *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c.Name() == "runs" {
			found = c
		}
	}
	if found == nil {
		t.Fatal(`no "runs" command on the root; it must register itself from this file's init()`)
	}
	if found.Args == nil {
		t.Error("runs takes no arguments and should say so")
	}
	for _, name := range []string{"json", "limit"} {
		if found.Flags().Lookup(name) == nil {
			t.Errorf("--%s flag is missing", name)
		}
	}
	if got := found.Flags().Lookup("limit").DefValue; got != "20" {
		t.Errorf("--limit default = %q, want %q", got, "20")
	}
}

func TestRunsRenderHonoursNoColorOnATerminal(t *testing.T) {
	// Not parallel: it sets environment variables.
	//
	// Every other render test gets uncoloured output for free, because a
	// bytes.Buffer is not a character device. This one checks the case
	// that actually needs the environment: a real terminal, where colour
	// is on until NO_COLOR turns it off.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	if !colorEnabled(devNull, false) {
		t.Skipf("%s is not a character device here, so there is no terminal to test against", os.DevNull)
	}

	t.Setenv("NO_COLOR", "1")
	if colorEnabled(devNull, false) {
		t.Fatal("NO_COLOR=1 did not switch colour off")
	}
	got := runsRender(runsRenderFixture(), 5, colorEnabled(devNull, false))
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("NO_COLOR=1 output still contains an escape sequence:\n%q", got)
	}
}
