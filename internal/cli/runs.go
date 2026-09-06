package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// Status labels shown by "belay runs". They are deliberately the words a
// person would use out loud rather than state.RunStatus's own spelling:
// a run is "finished", not "completed", and "stopped", not "aborted".
// RunSummary carries the manifest's own spelling alongside these in
// ManifestStatus, so a script never has to depend on this vocabulary.
const (
	runsStatusRunning    = "running"
	runsStatusPaused     = "paused"
	runsStatusFinished   = "finished"
	runsStatusFailed     = "failed"
	runsStatusStopped    = "stopped"
	runsStatusUnknown    = "unknown"
	runsStatusUnreadable = "unreadable"
)

// runsStatusLabels maps each lifecycle state to the word the listing shows.
var runsStatusLabels = map[state.RunStatus]string{
	state.RunStatusUnknown:   runsStatusUnknown,
	state.RunStatusRunning:   runsStatusRunning,
	state.RunStatusPaused:    runsStatusPaused,
	state.RunStatusCompleted: runsStatusFinished,
	state.RunStatusFailed:    runsStatusFailed,
	state.RunStatusAborted:   runsStatusStopped,
}

// runsProbeRunID is a throwaway run id used only to ask state.Layout where
// run directories live. Only the parent of the resulting path is used.
const runsProbeRunID = "probe"

// RunCost is one run's spend ledger as reported by "belay runs".
//
// It mirrors state.Budget rather than embedding it so that the JSON field
// names this command documents cannot drift when the on-disk ledger grows
// a field.
type RunCost struct {
	// SpentUSD is cumulative spend so far, in US dollars.
	SpentUSD float64 `json:"spent_usd"`
	// LimitUSD is the configured ceiling this run started with. It is 0
	// when the run was started without one.
	LimitUSD float64 `json:"limit_usd"`
	// Estimated reports that at least one contributing usage figure was
	// estimated rather than reported by the agent backend, which makes
	// SpentUSD a good guess rather than a fact.
	Estimated bool `json:"estimated"`
	// TokensIn is cumulative input tokens.
	TokensIn int64 `json:"tokens_in"`
	// TokensOut is cumulative output tokens.
	TokensOut int64 `json:"tokens_out"`
}

// RunSummary is one run as "belay runs" reports it, and is the documented
// shape of one element of the --json output's "runs" array.
//
// Every field is always present in JSON, including the zero values of a
// run whose manifest could not be read: a caller scripting against this
// shape should never have to distinguish a missing key from an empty one.
//
// Fields come in humanized/raw pairs where the two can differ. Status and
// Where are what the table prints; ManifestStatus and CurrentNode are what
// manifest.json actually holds. Consume the raw pair when comparing against
// the rest of belay, and the humanized pair when showing a person.
type RunSummary struct {
	// RunID is the run's identifier, which is also the name of its
	// directory. It is reported verbatim from the directory name rather
	// than from manifest.json, because the directory name is what a user
	// must type to resume or inspect the run, even if the manifest inside
	// disagrees with it.
	RunID string `json:"run_id"`
	// Unreadable reports that this run directory exists but belay could
	// not make sense of it. Every other field except RunID, Path, Status
	// and Reason is zero when it is true.
	Unreadable bool `json:"unreadable"`
	// Reason is a short, plain explanation of why Unreadable is true, such
	// as "no manifest.json". It is empty for a readable run.
	Reason string `json:"reason"`
	// Status is the lifecycle word the table prints: running, paused,
	// finished, failed, stopped, unknown, or unreadable.
	Status string `json:"status"`
	// ManifestStatus is state.RunStatus's own spelling of Status, such as
	// "completed" where Status says "finished". It is empty for an
	// unreadable run.
	ManifestStatus string `json:"manifest_status"`
	// Where is the humanized location the table prints, such as
	// "waiting for plan approval" for a run paused at the approve node.
	// It is empty when the run has no recorded node.
	Where string `json:"where"`
	// CurrentNode is the raw graph node name from manifest.json.
	CurrentNode string `json:"current_node"`
	// Step is the number of node executions the run has completed.
	Step int `json:"step"`
	// CreatedAt is when the run started. For an unreadable run it falls
	// back to the run directory's modification time, so that a damaged
	// run still sorts into roughly the right place instead of sinking to
	// the bottom of the list where nobody looks.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the run's manifest was last written.
	UpdatedAt time.Time `json:"updated_at"`
	// Goal is the run's objective in full, with control characters removed
	// and internal whitespace collapsed. The table truncates it; this does
	// not.
	Goal string `json:"goal"`
	// Resumable reports that "belay resume" has something to continue:
	// the run is paused, or it is marked running and therefore either live
	// in another process or crashed partway through. A manifest alone
	// cannot tell those two apart, so this is a "worth trying" signal
	// rather than a guarantee.
	Resumable bool `json:"resumable"`
	// Cost is the run's spend ledger.
	Cost RunCost `json:"cost"`
	// Path is the absolute path to the run directory.
	Path string `json:"path"`
}

// ScanRuns reads every run directory belonging to workspaceDir and returns
// one RunSummary per directory, newest first by CreatedAt.
//
// workspaceDir must be absolute. If the workspace has no runs directory at
// all, ScanRuns returns an empty slice and no error: a workspace nobody has
// run belay in yet is an ordinary state, not a failure.
//
// A run directory that cannot be read — no manifest.json, a truncated one,
// a schema version this build does not know, a name that is not a usable
// run id — is returned with Unreadable set and Reason explaining why,
// never omitted and never as an error. One damaged run must not be able to
// hide the healthy runs listed beside it, and a run that silently vanishes
// from the listing is worse than one that admits it is damaged.
//
// ScanRuns only ever reads manifest.json. It deliberately does not open
// state.json or journal.ndjson, which keeps the cost of listing runs
// proportional to the number of runs rather than to their length, and
// keeps a corrupt journal from affecting a listing that does not need it.
func ScanRuns(workspaceDir string) ([]RunSummary, error) {
	runsDir, err := runsDirOf(workspaceDir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(runsDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return []RunSummary{}, nil
	case err != nil:
		return nil, fmt.Errorf("cli: read runs directory %s: %w", runsDir, err)
	}

	summaries := make([]RunSummary, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(runsDir, entry.Name())
		// Stat rather than trusting entry.IsDir: it follows a symlinked
		// run directory, and its ModTime is the sort key for a run whose
		// manifest is unreadable and therefore has no CreatedAt.
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			continue
		}
		// UTC, because manifest timestamps are UTC and a JSON consumer
		// should not have to reconcile two offsets in one listing.
		summaries = append(summaries, runsSummarize(workspaceDir, entry.Name(), path, info.ModTime().UTC()))
	}

	// Newest first. Run ids begin with a UTC timestamp, so a descending id
	// is both a meaningful and a deterministic tiebreak for two runs that
	// share a CreatedAt -- which fixture runs routinely do.
	slices.SortStableFunc(summaries, func(a, b RunSummary) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.RunID, a.RunID)
	})
	return summaries, nil
}

// runsDirOf returns the directory holding every run of workspaceDir.
//
// It derives that path from state.Layout instead of joining ".belay/runs"
// here. internal/state owns the run directory layout, and a second copy of
// the path in the CLI is exactly how a listing command ends up confidently
// reporting "no runs yet" from the wrong directory after a layout change.
func runsDirOf(workspaceDir string) (string, error) {
	layout, err := state.NewLayout(workspaceDir, runsProbeRunID)
	if err != nil {
		return "", fmt.Errorf("cli: locate runs directory: %w", err)
	}
	return filepath.Dir(layout.RunDir()), nil
}

// runsSummarize builds one RunSummary from a run directory, degrading to an
// unreadable summary rather than failing.
func runsSummarize(workspaceDir, name, path string, modTime time.Time) RunSummary {
	id := sanitizeRunsText(name)

	layout, err := state.NewLayout(workspaceDir, name)
	if err != nil {
		return runsUnreadable(id, path, modTime, "not a usable run id")
	}

	manifest, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return runsUnreadable(id, path, modTime, runsUnreadableReason(err))
	}

	node := sanitizeRunsText(manifest.CurrentNode)
	return RunSummary{
		RunID:          id,
		Status:         runsStatusLabel(manifest.Status),
		ManifestStatus: manifest.Status.String(),
		Where:          runsWhere(manifest.Status, node),
		CurrentNode:    node,
		Step:           manifest.Step,
		CreatedAt:      manifest.CreatedAt,
		UpdatedAt:      manifest.UpdatedAt,
		Goal:           sanitizeRunsText(manifest.Goal),
		Resumable:      manifest.Status == state.RunStatusPaused || manifest.Status == state.RunStatusRunning,
		Cost: RunCost{
			SpentUSD:  manifest.Budget.SpentUSD,
			LimitUSD:  manifest.Budget.LimitUSD,
			Estimated: manifest.Budget.Estimated,
			TokensIn:  manifest.Budget.TokensIn,
			TokensOut: manifest.Budget.TokensOut,
		},
		Path: path,
	}
}

// runsUnreadable builds the summary for a run directory belay cannot read.
func runsUnreadable(id, path string, modTime time.Time, reason string) RunSummary {
	// Both timestamps are the directory's modification time. It is the
	// only true thing left to say about when this run happened, and it
	// beats emitting the zero time, which a script would have to know to
	// treat as a sentinel and a person would read as the year 1.
	return RunSummary{
		RunID:      id,
		Unreadable: true,
		Reason:     reason,
		Status:     runsStatusUnreadable,
		CreatedAt:  modTime,
		UpdatedAt:  modTime,
		Path:       path,
	}
}

// runsUnreadableReason turns a manifest load failure into a short phrase for a
// person reading a list.
//
// It deliberately does not surface the underlying error text. A listing is
// where someone re-enters work they left hours ago, and a wall of decoder
// internals next to their other runs reads as "something is badly wrong
// with belay" rather than "this one run is damaged". RunSummary.Path says
// where to look for anyone who wants the detail.
func runsUnreadableReason(err error) string {
	var versionErr *state.ManifestVersionError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no manifest.json"
	case errors.Is(err, fs.ErrPermission):
		return "manifest.json is not readable"
	case errors.As(err, &versionErr):
		return fmt.Sprintf("unsupported manifest version %d", versionErr.Got)
	default:
		return "manifest.json is damaged"
	}
}

// runsStatusLabel returns the word the listing prints for s.
func runsStatusLabel(s state.RunStatus) string {
	if label, ok := runsStatusLabels[s]; ok {
		return label
	}
	return runsStatusUnknown
}

// runsWhere describes where a run is, in words that tell the reader what is
// happening rather than which node happens to be current.
//
// A run paused on the approve node is the case that matters: "approve" is
// an instruction to belay, but the person reading the list is the one being
// waited on, and needs to be told so.
func runsWhere(status state.RunStatus, node string) string {
	switch {
	case node == "":
		return ""
	case status == state.RunStatusPaused && node == graph.NodeApprove:
		return "needs plan approval"
	case status == state.RunStatusPaused:
		return "waiting at " + node
	default:
		return node
	}
}

// sanitizeRunsText makes an arbitrary string safe and tidy to print in a
// table: control characters are dropped and every run of whitespace becomes
// a single space.
//
// Dropping control characters is a correctness requirement, not tidiness.
// A goal, a node name and a run directory name are all attacker- or
// accident-supplied, and any of them can carry an ESC. Printed raw, that
// rewrites the user's terminal -- including when belay has otherwise
// disabled color because NO_COLOR is set or output is piped to a file.
func sanitizeRunsText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
		case unicode.IsControl(r):
			// Dropped entirely, and deliberately not treated as a space:
			// an ESC is not whitespace and should leave no trace.
		default:
			if pendingSpace {
				b.WriteRune(' ')
				pendingSpace = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Table geometry.
const (
	// runsGoalWidth caps the goal column. The goal is what a person
	// actually recognises a run by, so it gets the widest column, but it
	// is also the only unbounded field on the row and so is the one that
	// has to be cut. It is last for the same reason: when a narrow
	// terminal wraps the line, the column that wraps is the one whose
	// exact tail matters least.
	runsGoalWidth = 36
	// runsGutter separates columns. Two spaces is enough to read as a
	// break without letting the eye lose the row on the way across.
	runsGutter = "  "
	// runsMissing stands in for a value a run does not have.
	runsMissing = "—"
)

// RunsListing is the documented shape of "belay runs --json".
//
// It is an object rather than a bare array so that truncation is visible to
// a script for the same reason it is visible to a person: a list that
// quietly stops at 20 is worse than one that says it did.
type RunsListing struct {
	// Workspace is the absolute path the runs were listed from.
	Workspace string `json:"workspace"`
	// Total is how many runs the workspace holds, before --limit.
	Total int `json:"total"`
	// Shown is how many runs appear in Runs.
	Shown int `json:"shown"`
	// Hidden is Total minus Shown: the runs --limit held back.
	Hidden int `json:"hidden"`
	// Runs are the runs themselves, newest first. It is never null.
	Runs []RunSummary `json:"runs"`
}

// runsColumn is one column of the table: its heading, how its cells are
// aligned, whether it carries emphasis, and the plain text of every cell.
//
// Cells are held as plain text so that column widths are measured before
// any escape sequence is added. Measuring after would count the escapes as
// visible characters and skew every column to their right -- which is the
// same reason this code pads by hand rather than using text/tabwriter,
// whose Escape mechanism still counts the escaped bytes toward cell width.
type runsColumn struct {
	header string
	right  bool
	emph   bool
	cells  []string
}

// runsTableColumns lays out summaries as columns, newest run first.
func runsTableColumns(rows []RunSummary, now time.Time) []runsColumn {
	spentWidth := 0
	for _, r := range rows {
		if !r.Unreadable {
			spentWidth = max(spentWidth, runsRuneLen(runsSpent(r.Cost)))
		}
	}

	cols := []runsColumn{
		{header: "RUN ID"},
		{header: "STATUS", emph: true},
		{header: "WHERE"},
		{header: "STEPS", right: true},
		{header: "STARTED", right: true},
		{header: "COST", emph: true},
		{header: "GOAL"},
	}
	for i := range cols {
		cols[i].cells = make([]string, 0, len(rows))
	}

	for _, r := range rows {
		steps, goal := runsMissing, r.Goal
		if !r.Unreadable {
			steps = fmt.Sprintf("%d", r.Step)
		} else {
			// A damaged run's last column explains the damage: it is the
			// same question the goal answers for every other row -- what
			// is this run? -- and the answer here is "not readable, for
			// this reason".
			goal = r.Reason
		}
		cols[0].cells = append(cols[0].cells, r.RunID)
		cols[1].cells = append(cols[1].cells, r.Status)
		cols[2].cells = append(cols[2].cells, runsOrMissing(r.Where))
		cols[3].cells = append(cols[3].cells, steps)
		cols[4].cells = append(cols[4].cells, runsAge(now, r.CreatedAt))
		cols[5].cells = append(cols[5].cells, runsCostCell(r, spentWidth))
		cols[6].cells = append(cols[6].cells, runsTruncate(goal, runsGoalWidth))
	}
	return cols
}

// renderRunsTable writes the table, then the notes that make it actionable.
func renderRunsTable(u *ui, rows []RunSummary, total int, now time.Time) {
	cols := runsTableColumns(rows, now)

	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = runsRuneLen(c.header)
		for _, cell := range c.cells {
			widths[i] = max(widths[i], runsRuneLen(cell))
		}
	}

	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = c.header
	}
	u.say("%s", runsLine(u, cols, widths, header, false))

	for row := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = c.cells[row]
		}
		u.say("%s", runsLine(u, cols, widths, cells, true))
	}

	for _, note := range runsFooter(rows, total) {
		u.say("%s", note)
	}
}

// runsLine pads one row of cells to the column widths and joins them.
//
// Emphasis is applied after padding is measured, never before, so an escape
// sequence can never contribute to a column's width.
func runsLine(u *ui, cols []runsColumn, widths []int, cells []string, style bool) string {
	var b strings.Builder
	for i, cell := range cells {
		if i > 0 {
			b.WriteString(runsGutter)
		}
		pad := strings.Repeat(" ", max(0, widths[i]-runsRuneLen(cell)))
		text := cell
		if style && cols[i].emph {
			text = u.emph(text)
		}
		if cols[i].right {
			b.WriteString(pad)
			b.WriteString(text)
		} else {
			b.WriteString(text)
			b.WriteString(pad)
		}
	}
	// The last column is padded like the others above; trimming here keeps
	// the file a pipe receives free of trailing whitespace.
	return strings.TrimRight(b.String(), " ")
}

// runsFooter returns the lines that follow the table: what was hidden, what
// was estimated, and what to type next.
//
// A paused run that the reader cannot act on is only half a listing, so the
// last line is the command itself, with the run id already filled in.
func runsFooter(rows []RunSummary, total int) []string {
	var notes []string

	if hidden := total - len(rows); hidden > 0 {
		notes = append(notes, fmt.Sprintf("%s not shown. Pass --limit %d to see them all, or --limit 0 for no cap.",
			runsPlural(hidden, "more run", "more runs"), total))
	}

	for _, r := range rows {
		if r.Cost.Estimated {
			notes = append(notes, "~ marks a cost that includes estimated usage rather than reported figures.")
			break
		}
	}

	var paused []RunSummary
	for _, r := range rows {
		if r.Status == runsStatusPaused {
			paused = append(paused, r)
		}
	}
	if len(paused) > 0 {
		// rows are newest first, so paused[0] is the run the reader is
		// most likely to have come back for.
		hint := fmt.Sprintf("Resume the paused run:  belay resume %s", paused[0].RunID)
		if len(paused) > 1 {
			hint = fmt.Sprintf("%s are paused. Resume the newest:  belay resume %s",
				runsPlural(len(paused), "run", "runs"), paused[0].RunID)
		}
		notes = append(notes, hint)
	}

	// One blank line sets the notes apart from the rows above them: the
	// table is the data, these are remarks about it.
	if len(notes) > 0 {
		notes = append([]string{""}, notes...)
	}
	return notes
}

// runsPlural renders a count with the right noun, because "1 runs" is the
// kind of small wrongness that makes a tool feel unfinished.
func runsPlural(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// runsSpent renders what a run has spent, marked when the figure rests on
// an estimate.
//
// The tilde is not decoration. The ledger records whether any contributing
// usage was estimated, and a reader deciding whether a run is worth
// continuing should never be handed a guess formatted exactly like a fact.
func runsSpent(c RunCost) string {
	if c.Estimated {
		return fmt.Sprintf("~$%.2f", c.SpentUSD)
	}
	return fmt.Sprintf("$%.2f", c.SpentUSD)
}

// runsCostCell renders spend against its ceiling, with the spent figure
// right-aligned inside spentWidth so that the decimal points line up down
// the column even when the amounts differ in magnitude.
func runsCostCell(r RunSummary, spentWidth int) string {
	if r.Unreadable {
		// Padded to the same width as a real figure so the marker sits in
		// the money column rather than beside it.
		return fmt.Sprintf("%*s", spentWidth, runsMissing)
	}
	spent := fmt.Sprintf("%*s", spentWidth, runsSpent(r.Cost))
	if r.Cost.LimitUSD <= 0 {
		return spent + " (no limit)"
	}
	return fmt.Sprintf("%s of $%.2f", spent, r.Cost.LimitUSD)
}

// runsAge renders how long ago a run started, the way a person thinks about
// recency: nobody remembers a run by its timestamp, but everybody knows
// whether they were working on something twenty minutes or two weeks ago.
//
// A run whose start time is unknown renders as missing rather than as a
// confident age counted from the zero time, which would read as decades.
func runsAge(now, then time.Time) string {
	if then.IsZero() {
		return runsMissing
	}
	d := now.Sub(then)
	switch {
	// A clock that has moved backwards, or a run started in the same
	// second, are both "just now" -- never a negative age.
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return runsPlural(int(d/time.Minute), "minute ago", "minutes ago")
	case d < 24*time.Hour:
		return runsPlural(int(d/time.Hour), "hour ago", "hours ago")
	case d < 7*24*time.Hour:
		return runsPlural(int(d/(24*time.Hour)), "day ago", "days ago")
	case d < 35*24*time.Hour:
		return runsPlural(int(d/(7*24*time.Hour)), "week ago", "weeks ago")
	case d < 365*24*time.Hour:
		return runsPlural(int(d/(30*24*time.Hour)), "month ago", "months ago")
	default:
		return runsPlural(int(d/(365*24*time.Hour)), "year ago", "years ago")
	}
}

// runsTruncate shortens s to at most maxRunes runes, marking the cut.
func runsTruncate(s string, maxRunes int) string {
	if maxRunes <= 0 || runsRuneLen(s) <= maxRunes {
		return s
	}
	kept := make([]rune, 0, maxRunes)
	for _, r := range s {
		if len(kept) == maxRunes-1 {
			break
		}
		kept = append(kept, r)
	}
	return string(kept) + "…"
}

// runsOrMissing renders an empty value as the missing marker, so that a blank
// cell is never mistaken for a column that failed to print.
func runsOrMissing(s string) string {
	if s == "" {
		return runsMissing
	}
	return s
}

// runsRuneLen counts display cells as this table measures them: one per rune.
func runsRuneLen(s string) int { return utf8.RuneCountInString(s) }

// runsDefaultLimit caps the listing at roughly a screen's worth of runs.
// The cap exists so the useful runs -- the recent ones -- are not pushed
// off the top of a scrollback by months of finished ones, and the footer
// always says how many it held back.
const runsDefaultLimit = 20

// runsFlags holds the settings belonging to "belay runs" alone.
var runsFlags struct {
	limit int
	json  bool
}

var runsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List this workspace's runs, newest first",
	Long: `List the runs belay has in this workspace, newest first.

Each row is one run: its id, how it ended or where it stopped, how far it
got, when it started, and what it has spent against its ceiling. A cost
marked with a tilde includes estimated usage rather than reported figures.

A run directory belay cannot read is listed as "unreadable" rather than
skipped, so a damaged run is never silently missing from the list.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		workspace, err := runsWorkspaceDir(cmd)
		if err != nil {
			return err
		}
		return runRuns(newUI(cmd), runsOptions{
			workspace: workspace,
			limit:     runsFlags.limit,
			asJSON:    runsFlags.json,
			now:       time.Now(),
		})
	},
}

func init() {
	f := runsCmd.Flags()
	f.IntVar(&runsFlags.limit, "limit", runsDefaultLimit,
		"list at most this many runs, newest first (0 for no cap)")
	f.BoolVar(&runsFlags.json, "json", false,
		"print the listing as JSON instead of a table")

	rootCmd.AddCommand(runsCmd)
}

// runsOptions is everything "belay runs" needs to produce its output.
//
// The workspace and the clock are passed in rather than read from the
// process so that the command's behaviour is fully determined by its
// inputs, which is what lets its output be asserted exactly.
type runsOptions struct {
	workspace string
	limit     int
	asJSON    bool
	now       time.Time
}

// runRuns lists the workspace's runs.
//
// It returns an error only when the listing itself could not be produced.
// A workspace with no runs, and a workspace whose runs are damaged, are
// both ordinary outcomes reported on stdout with a zero exit status: the
// question "what have I got going?" has been answered either way.
func runRuns(u *ui, opts runsOptions) error {
	if opts.limit < 0 {
		// No package prefix: Execute prints this straight to the person
		// who mistyped the flag, and "belay: cli: --limit ..." tells them
		// nothing that "belay: --limit ..." does not.
		return fmt.Errorf("--limit must be 0 or more, not %d", opts.limit)
	}

	all, err := ScanRuns(opts.workspace)
	if err != nil {
		return err
	}

	shown := all
	if opts.limit > 0 && len(shown) > opts.limit {
		shown = shown[:opts.limit]
	}

	if opts.asJSON {
		return writeRunsJSON(u.out, RunsListing{
			Workspace: opts.workspace,
			Total:     len(all),
			Shown:     len(shown),
			Hidden:    len(all) - len(shown),
			Runs:      shown,
		})
	}

	if len(all) == 0 {
		// An empty workspace is a state, not a failure, and it is worth a
		// sentence rather than a blank table: the reader needs to know
		// belay looked and found nothing, not wonder whether it looked.
		u.say("No runs yet in %s. Start one with belay run.", opts.workspace)
		return nil
	}

	renderRunsTable(u, shown, len(all), opts.now)
	return nil
}

// writeRunsJSON writes the listing as indented JSON.
func writeRunsJSON(w io.Writer, listing RunsListing) error {
	// Never null: a script that reads .runs should always find an array,
	// including in a workspace that has never been run in.
	if listing.Runs == nil {
		listing.Runs = []RunSummary{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(listing); err != nil {
		return fmt.Errorf("cli: write runs JSON: %w", err)
	}
	return nil
}

// runsWorkspaceDir resolves the workspace to list runs from: the shared
// --workspace flag, defaulting to the directory belay was invoked in.
//
// It is made absolute here rather than left to state.Layout because the path
// is also printed -- "No runs yet in ." tells the reader nothing about where
// belay actually looked.
func runsWorkspaceDir(cmd *cobra.Command) (string, error) {
	return workspaceDirOf(cmd)
}
