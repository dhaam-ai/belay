package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/spf13/cobra"
)

// TimelineOutcome is the plain-language result of one node execution, as the
// timeline says it out loud. It deliberately does not reuse
// journal.Status.String(): the journal's vocabulary is the type system's
// ("aborted", "ok") and the timeline's is the reader's ("stopped",
// "finished"), per ADR 0010's fourth cognitive system.
type TimelineOutcome string

const (
	// TimelineFinished is a node that ran to a successful end.
	TimelineFinished TimelineOutcome = "finished"
	// TimelineFailed is a node that ran and produced a failing result. It is
	// not an error in the timeline's own operation: a failing test node is
	// exactly what a fix loop is for.
	TimelineFailed TimelineOutcome = "failed"
	// TimelinePaused is a node that suspended the run, such as an approval
	// gate waiting on a human.
	TimelinePaused TimelineOutcome = "paused"
	// TimelineStopped is a node whose execution was cut short.
	TimelineStopped TimelineOutcome = "stopped"
	// TimelineInterrupted is a node that started and never recorded an end,
	// because the process died while it was running. It is the expected
	// shape of a crash, not a defect.
	TimelineInterrupted TimelineOutcome = "interrupted"
)

// timelineOutcome translates a journal status into the reader's vocabulary.
func timelineOutcome(s journal.Status) TimelineOutcome {
	switch s {
	case journal.StatusOK:
		return TimelineFinished
	case journal.StatusFailed:
		return TimelineFailed
	case journal.StatusPaused:
		return TimelinePaused
	case journal.StatusAborted:
		return TimelineStopped
	case journal.StatusUnknown:
		return TimelineInterrupted
	default:
		return TimelineInterrupted
	}
}

// timelineRunState translates a resolved run state into the reader's
// vocabulary. A run that is merely runnable has not reached any terminal
// state, so it reads as unfinished rather than as a status word.
func timelineRunState(s journal.RunState) string {
	switch s {
	case journal.RunStateCompleted:
		return "finished"
	case journal.RunStateFailed:
		return "failed"
	case journal.RunStatePaused:
		return "paused, waiting to be resumed"
	case journal.RunStateAborted:
		return "stopped"
	case journal.RunStateRunnable:
		return "unfinished"
	default:
		return "unfinished"
	}
}

// TimelineEntry is one node execution: one row of the rendered timeline.
type TimelineEntry struct {
	// Index is the entry's 1-based position in the run, so a reader can
	// refer to "step 7" without counting rows.
	Index int
	// Node is the graph node that ran.
	Node string
	// Iteration is which visit to Node this is, 1-based. Together with
	// Iterations it turns a repeated node into "test #2/3" rather than
	// leaving the reader to notice the repetition themselves.
	Iteration int
	// Iterations is the total number of visits to Node across the whole run.
	Iterations int
	// Attempt is the journal's attempt counter for this visit. It is greater
	// than 1 only when a previous attempt of this same visit died without
	// finishing — a crash recovery, not a loop iteration.
	Attempt int
	// Outcome is what happened, in the reader's vocabulary.
	Outcome TimelineOutcome
	// StartedAt is when the node began, from its node_started record. It is
	// the zero time when that record was lost to a torn tail.
	StartedAt time.Time
	// FinishedAt is when the node ended. It is the zero time for an entry
	// that never finished.
	FinishedAt time.Time
	// Duration is how long the node ran. It is zero for an entry that never
	// finished, which is not the same as a node that finished instantly —
	// Outcome distinguishes them.
	Duration time.Duration
	// Usage is the cost accounting for this execution, or nil when the node
	// was never billed at all (an approval gate, a local file write). A
	// non-nil Usage with a zero USD means "accounted, and it was free".
	Usage *journal.Usage
	// Next is the successor the dispatcher chose after this node.
	Next string
	// Note is the record's free-form text, already redacted upstream.
	Note string
}

// Repeated reports whether Node ran more than once in this run, and so
// whether the entry should be numbered as a loop iteration.
func (e TimelineEntry) Repeated() bool { return e.Iterations > 1 }

// Rerun reports whether this entry is a second or later attempt at the same
// visit to the node, meaning an earlier attempt died mid-execution.
func (e TimelineEntry) Rerun() bool { return e.Attempt > 1 }

// TimelineMarker is a run-level event between two node executions: the
// places where the run stopped being about the agent and started being about
// a human. Markers exist to explain wall-clock gaps that node durations
// alone cannot.
type TimelineMarker struct {
	// AfterIndex is the entry index this marker follows; 0 means it precedes
	// every entry.
	AfterIndex int
	// Kind is "paused" or "resumed".
	Kind string
	// Time is when the marker was recorded.
	Time time.Time
	// Gap is, for a "resumed" marker, how long the run sat paused. It is
	// zero for every other kind.
	Gap time.Duration
}

// TimelineLoop names a node that ran more than once, and how many times.
// It is derived from journal.Stats, which already counts executions per
// node, so the timeline never recounts what the journal package derives.
type TimelineLoop struct {
	// Node is the repeated node.
	Node string
	// Executions is how many times it completed.
	Executions int
}

// TimelineTotals is the arithmetic the reader would otherwise have to do in
// their head: the whole run's time and money in one place.
type TimelineTotals struct {
	// Nodes is the number of node executions in the timeline, including one
	// that was interrupted mid-flight.
	Nodes int
	// AgentDuration is the sum of every finished node's duration.
	AgentDuration time.Duration
	// WallDuration is first record to last record: elapsed real time,
	// including any stretch the run spent paused waiting on a human.
	WallDuration time.Duration
	// USD is total attributed spend across every node.
	USD float64
	// Estimated is true when any contributing figure was itself an estimate,
	// and so the total is only as precise as its least precise part.
	Estimated bool
	// TokensIn and TokensOut are cumulative token counts.
	TokensIn, TokensOut int64
	// BudgetLimitUSD is the ceiling this run started with, from its
	// manifest, or 0 when no manifest was available or none was set.
	BudgetLimitUSD float64
	// LoopUSD and LoopDuration are what the run spent going round: the cost
	// and time of every step from the first execution of a repeated node to
	// the last, inclusive.
	//
	// The span matters rather than the repeated nodes alone. A fix that
	// worked first time still ran only because a test failed, so charging
	// the loop only for nodes that literally repeated would report a repair
	// loop as nearly free — which is the opposite of what someone deciding
	// whether belay is worth its cost needs to know.
	LoopUSD      float64
	LoopDuration time.Duration
	// LoopFirstIndex and LoopLastIndex bracket that span in entry indices,
	// so the figure can point at the rows it came from. Both are 0 when
	// nothing looped.
	LoopFirstIndex, LoopLastIndex int
}

// Timeline is one run's execution order, with the timing, cost and loop
// structure needed to answer the only two questions anyone opens a timeline
// to answer: why did this take so long or cost so much, and what went wrong.
type Timeline struct {
	// Run identifies which run this is and how it was chosen.
	Run TimelineSelection
	// Goal is the run's objective from its manifest, or empty when no
	// manifest could be read.
	Goal string
	// Status is the run's overall state in the reader's vocabulary.
	Status string
	// Next is the node that would run next, populated only while the run is
	// unfinished and the journal is well-formed enough to say.
	Next string
	// NextAttempt is the attempt number Next would use.
	NextAttempt int
	// StartedAt and EndedAt bracket the run's recorded history.
	StartedAt, EndedAt time.Time
	// Entries is every node execution, in the order they happened.
	Entries []TimelineEntry
	// Markers are the pause and resume points between entries.
	Markers []TimelineMarker
	// Loops names every node that ran more than once.
	Loops []TimelineLoop
	// Totals is the whole-run arithmetic.
	Totals TimelineTotals
	// Interrupted is true when the run stopped without recording an ending:
	// a node left open, a final journal line torn mid-write, or both.
	Interrupted bool
	// InterruptedNode is the node the run was inside when it stopped, or the
	// last node that completed before the record stops.
	InterruptedNode string
	// InterruptedMidWrite is true when the journal's final line was itself
	// incomplete. That is what a durable append-only journal looks like when
	// the process is killed between write and the next record: expected, and
	// reported as such.
	InterruptedMidWrite bool
}

// InterruptedMidNode reports whether the run stopped with a node still
// running — a node_started that never got its node_finished. It is false when
// the interruption is known only from a torn final line, because then even
// which node was starting was lost with that line.
func (t Timeline) InterruptedMidNode() bool {
	if !t.Interrupted || len(t.Entries) == 0 {
		return false
	}
	return t.Entries[len(t.Entries)-1].Outcome == TimelineInterrupted
}

// TimelineSelection records which run the timeline is about and how that run
// was chosen. A command that acts on an implicit target must be able to say
// which target it picked, so the selection travels with the data.
type TimelineSelection struct {
	// RunID is the run's identifier.
	RunID string
	// Dir is the run's directory on disk.
	Dir string
	// Workspace is the repository the run belongs to.
	Workspace string
	// Implicit is true when no run id was given on the command line and the
	// most recent run was chosen.
	Implicit bool
	// TotalRuns is how many runs exist in the workspace, so an implicit
	// choice can say what it chose among.
	TotalRuns int
}

// BuildTimeline assembles a Timeline from one run's journal records.
//
// It never fails. A journal that is torn, truncated or internally
// inconsistent still produces a timeline of everything that is legible,
// because a reader inspecting a crashed run is exactly the reader who most
// needs the view to work. manifest may be nil when none could be read; the
// goal and budget ceiling are simply absent then.
func BuildTimeline(sel TimelineSelection, manifest *state.Manifest, res journal.ReadResult) Timeline {
	tl := Timeline{
		Run:                 sel,
		Interrupted:         res.Incomplete,
		InterruptedMidWrite: res.Incomplete,
	}
	if manifest != nil {
		tl.Goal = manifest.Goal
		tl.Totals.BudgetLimitUSD = manifest.Budget.LimitUSD
	}

	// journal.Stats already derives per-node execution counts and summed
	// durations, and is documented to degrade rather than fail on a damaged
	// history. Loop detection and total agent time come from it verbatim.
	for _, s := range journal.Stats(res.Records) {
		tl.Totals.AgentDuration += s.TotalDuration
		if s.Executions > 1 {
			tl.Loops = append(tl.Loops, TimelineLoop{Node: s.Node, Executions: s.Executions})
		}
	}

	visits := make(map[string]int)
	var (
		open     *journal.Record
		pausedAt time.Time
	)
	for i := range res.Records {
		rec := res.Records[i]
		switch rec.Event {
		case journal.EventNodeStarted:
			// rec is declared fresh each iteration, so taking its address
			// captures this record and not the next one.
			open = &rec
		case journal.EventNodeFinished:
			started := time.Time{}
			if open != nil && open.Node == rec.Node && open.Attempt == rec.Attempt {
				started = open.Time
			}
			open = nil
			visits[rec.Node]++
			tl.Entries = append(tl.Entries, TimelineEntry{
				Index:      len(tl.Entries) + 1,
				Node:       rec.Node,
				Iteration:  visits[rec.Node],
				Attempt:    rec.Attempt,
				Outcome:    timelineOutcome(rec.Status),
				StartedAt:  started,
				FinishedAt: rec.Time,
				Duration:   time.Duration(rec.DurationMS) * time.Millisecond,
				Usage:      rec.Usage,
				Next:       rec.Next,
				Note:       rec.Note,
			})
			if u := rec.Usage; u != nil {
				tl.Totals.USD += u.USD
				tl.Totals.TokensIn += u.InputTokens
				tl.Totals.TokensOut += u.OutputTokens
				tl.Totals.Estimated = tl.Totals.Estimated || u.Estimated
			}
		case journal.EventRunPaused:
			pausedAt = rec.Time
			tl.Markers = append(tl.Markers, TimelineMarker{
				AfterIndex: len(tl.Entries), Kind: "paused", Time: rec.Time,
			})
		case journal.EventRunResumed:
			m := TimelineMarker{AfterIndex: len(tl.Entries), Kind: "resumed", Time: rec.Time}
			if !pausedAt.IsZero() && rec.Time.After(pausedAt) {
				m.Gap = rec.Time.Sub(pausedAt)
			}
			pausedAt = time.Time{}
			tl.Markers = append(tl.Markers, m)
		case journal.EventUnknown, journal.EventRunStarted, journal.EventRunCompleted,
			journal.EventRunFailed, journal.EventRunAborted:
			// Run-level boundaries. They set the wall clock and the status,
			// both read below, and earn no row of their own.
		}
	}

	// A node_started with no matching node_finished is the signature of a
	// process killed while that node was running. It is a real execution and
	// belongs in the order, with the honest admission that its duration and
	// cost were never recorded.
	if open != nil {
		visits[open.Node]++
		tl.Interrupted = true
		tl.InterruptedNode = open.Node
		tl.Entries = append(tl.Entries, TimelineEntry{
			Index:     len(tl.Entries) + 1,
			Node:      open.Node,
			Iteration: visits[open.Node],
			Attempt:   open.Attempt,
			Outcome:   TimelineInterrupted,
			StartedAt: open.Time,
		})
	}

	for i := range tl.Entries {
		tl.Entries[i].Iterations = visits[tl.Entries[i].Node]
	}
	tl.Totals.Nodes = len(tl.Entries)

	if n := len(res.Records); n > 0 {
		tl.StartedAt = res.Records[0].Time
		tl.EndedAt = res.Records[n-1].Time
		if tl.EndedAt.After(tl.StartedAt) {
			tl.Totals.WallDuration = tl.EndedAt.Sub(tl.StartedAt)
		}
	}

	tl.applyLoopCost()
	tl.applyRunState(res.Records, manifest)
	return tl
}

// applyLoopCost measures the stretch of the run spent looping: from the first
// execution of a node that runs more than once to the last, inclusive.
func (t *Timeline) applyLoopCost() {
	if len(t.Loops) == 0 {
		return
	}
	looping := make(map[string]bool, len(t.Loops))
	for _, l := range t.Loops {
		looping[l.Node] = true
	}

	first, last := -1, -1
	for i, e := range t.Entries {
		if !looping[e.Node] {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return
	}

	for _, e := range t.Entries[first : last+1] {
		t.Totals.LoopDuration += e.Duration
		if e.Usage != nil {
			t.Totals.LoopUSD += e.Usage.USD
		}
	}
	t.Totals.LoopFirstIndex = t.Entries[first].Index
	t.Totals.LoopLastIndex = t.Entries[last].Index
}

// applyRunState fills in Status, Next and the interruption node from the
// resolved resume decision, falling back to the manifest and then to a plain
// "unfinished" when the journal is too damaged for journal.ResolveStart to
// commit to an answer.
func (t *Timeline) applyRunState(records []journal.Record, manifest *state.Manifest) {
	switch res, err := journal.ResolveStart(records); {
	case err != nil:
		// ResolveStart refuses to guess at a corrupt history. The timeline
		// still shows every legible row; it just declines to claim a state.
		t.Status = "unfinished"
		if manifest != nil {
			t.Status = timelineManifestStatus(manifest.Status)
		}
	case res.State == journal.RunStateRunnable:
		t.Status = timelineRunState(res.State)
		t.Next, t.NextAttempt = res.Node, res.Attempt
	default:
		t.Status = timelineRunState(res.State)
	}

	if t.Interrupted {
		t.Status = "interrupted"
		if t.InterruptedNode == "" && len(t.Entries) > 0 {
			t.InterruptedNode = t.Entries[len(t.Entries)-1].Node
		}
	}
	if len(t.Entries) == 0 && !t.Interrupted {
		t.Next, t.NextAttempt = "", 0
	}
}

// timelineManifestStatus translates a manifest run status into the reader's
// vocabulary. It is the fallback for a journal too damaged to resolve.
func timelineManifestStatus(s state.RunStatus) string {
	switch s {
	case state.RunStatusCompleted:
		return "finished"
	case state.RunStatusFailed:
		return "failed"
	case state.RunStatusPaused:
		return "paused, waiting to be resumed"
	case state.RunStatusAborted:
		return "stopped"
	case state.RunStatusRunning:
		return "unfinished"
	case state.RunStatusUnknown:
		return "unfinished"
	default:
		return "unfinished"
	}
}

// ErrTimelineRunNotFound reports that the requested run does not exist in the
// workspace. errors.Is matches every not-found shape against it.
var ErrTimelineRunNotFound = errors.New("cli: run not found")

// TimelineRunNotFoundError explains, in a sentence a first-time user can act
// on, that a run could not be found: which id was asked for, where belay
// looked, and how many runs are actually there.
type TimelineRunNotFoundError struct {
	// RunID is the id that was asked for, or empty when none was given and
	// the workspace turned out to have no runs at all.
	RunID string
	// RunsDir is the directory that was searched.
	RunsDir string
	// Available is how many runs that directory does contain.
	Available int
}

// Error returns the full sentence, including the next command to try. It
// names the missing id rather than only reporting a failure, because the id
// is usually a typo or a stale copy-paste and seeing it is the fix.
func (e *TimelineRunNotFoundError) Error() string {
	switch {
	case e.RunID == "":
		return fmt.Sprintf("no runs found in %s — start one with `belay run \"<goal>\"`", e.RunsDir)
	case e.Available == 0:
		return fmt.Sprintf("no run named %q in %s, which has no runs yet — start one with `belay run \"<goal>\"`",
			e.RunID, e.RunsDir)
	case e.Available == 1:
		return fmt.Sprintf("no run named %q in %s — run `belay runs` to see the one run that is there",
			e.RunID, e.RunsDir)
	default:
		return fmt.Sprintf("no run named %q in %s — run `belay runs` to see the %d runs that are there",
			e.RunID, e.RunsDir, e.Available)
	}
}

// Unwrap exposes ErrTimelineRunNotFound for errors.Is.
func (e *TimelineRunNotFoundError) Unwrap() error { return ErrTimelineRunNotFound }

// timelineRunsDir returns the directory holding a workspace's runs.
//
// internal/state documents that Layout is the only thing entitled to spell
// ".belay/runs/...", so this derives the parent of a probe run's directory
// rather than re-concatenating that shape here. The probe id is never
// touched on disk.
func timelineRunsDir(workspace string) (string, error) {
	probe, err := state.NewLayout(workspace, "probe")
	if err != nil {
		return "", err
	}
	return filepath.Dir(probe.RunDir()), nil
}

// timelineCandidate is one run directory found in a workspace, with the
// creation time used to order it.
type timelineCandidate struct {
	runID     string
	createdAt time.Time
}

// timelineListRuns returns every run directory in workspace, most recent
// first.
//
// Ordering prefers the manifest's CreatedAt and falls back to the run id,
// which state.NewRunID builds from a UTC timestamp and therefore already
// sorts chronologically. A run whose manifest is missing or unreadable is
// still listed: `belay timeline` must work on exactly the damaged run
// directory a user is trying to understand.
func timelineListRuns(workspace string) ([]timelineCandidate, string, error) {
	runsDir, err := timelineRunsDir(workspace)
	if err != nil {
		return nil, "", err
	}
	dirents, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, runsDir, nil
		}
		return nil, runsDir, fmt.Errorf("cli: read runs directory %s: %w", runsDir, err)
	}

	out := make([]timelineCandidate, 0, len(dirents))
	for _, de := range dirents {
		if !de.IsDir() {
			continue
		}
		c := timelineCandidate{runID: de.Name()}
		if m, ok := timelineLoadManifest(workspace, c.runID); ok {
			c.createdAt = m.CreatedAt
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b timelineCandidate) int {
		if !a.createdAt.Equal(b.createdAt) {
			return b.createdAt.Compare(a.createdAt)
		}
		return strings.Compare(b.runID, a.runID)
	})
	return out, runsDir, nil
}

// timelineLoadManifest reads a run's manifest, reporting failure as "absent"
// rather than as an error: a manifest supplies the goal text and the budget
// ceiling, both of which the timeline can do without.
func timelineLoadManifest(workspace, runID string) (state.Manifest, bool) {
	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		return state.Manifest{}, false
	}
	m, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return state.Manifest{}, false
	}
	return m, true
}

// ResolveTimelineRun decides which run the timeline is about.
//
// An empty runID selects the most recent run and marks the selection
// implicit, so the caller can report the choice instead of acting silently on
// a target the user never named. A runID that names no directory produces a
// *TimelineRunNotFoundError.
//
// workspace must be absolute; state.NewLayout rejects anything else, since a
// relative workspace silently resolves against whatever directory belay
// happens to be running in.
func ResolveTimelineRun(workspace, runID string) (TimelineSelection, error) {
	runs, runsDir, err := timelineListRuns(workspace)
	if err != nil {
		return TimelineSelection{}, err
	}

	if runID == "" {
		if len(runs) == 0 {
			return TimelineSelection{}, &TimelineRunNotFoundError{RunsDir: runsDir}
		}
		layout, err := state.NewLayout(workspace, runs[0].runID)
		if err != nil {
			return TimelineSelection{}, err
		}
		return TimelineSelection{
			RunID: layout.RunID(), Dir: layout.RunDir(), Workspace: workspace,
			Implicit: true, TotalRuns: len(runs),
		}, nil
	}

	// NewLayout validates the id rather than sanitizing it, which is what
	// keeps a run id of "../../etc" from being quietly reinterpreted.
	layout, err := state.NewLayout(workspace, runID)
	if err != nil {
		return TimelineSelection{}, err
	}
	if !slices.ContainsFunc(runs, func(c timelineCandidate) bool { return c.runID == runID }) {
		return TimelineSelection{}, &TimelineRunNotFoundError{
			RunID: runID, RunsDir: runsDir, Available: len(runs),
		}
	}
	return TimelineSelection{
		RunID: layout.RunID(), Dir: layout.RunDir(), Workspace: workspace, TotalRuns: len(runs),
	}, nil
}

// LoadTimeline resolves a run in workspace and builds its timeline from the
// journal on disk.
//
// A journal that is absent reads as an empty history rather than an error
// (journal.ReadFile's own contract), so a run directory that exists but never
// got as far as its first node still renders.
func LoadTimeline(workspace, runID string) (Timeline, error) {
	sel, err := ResolveTimelineRun(workspace, runID)
	if err != nil {
		return Timeline{}, err
	}
	layout, err := state.NewLayout(workspace, sel.RunID)
	if err != nil {
		return Timeline{}, err
	}
	res, err := journal.ReadFile(layout.JournalPath())
	if err != nil {
		// A malformed line partway through the file is real corruption, not
		// a torn tail, and the records before it are still worth showing.
		if !errors.Is(err, journal.ErrMalformedRecord) {
			return Timeline{}, err
		}
	}

	var manifest *state.Manifest
	if m, ok := timelineLoadManifest(workspace, sel.RunID); ok {
		manifest = &m
	}
	return BuildTimeline(sel, manifest, res), nil
}

// timelineNoValue is what the timeline prints where a number would be
// misleading: a node that was never billed, or one whose duration was never
// recorded because it did not finish. Printing "0.0s" or "$0.000" there
// would read as a measurement rather than as an absence.
const timelineNoValue = "–"

// timelineBarWidth is the widest a duration bar gets. The bar exists so the
// eye finds the expensive node without reading any digits; past roughly this
// width it stops being scannable and starts being a wall.
const timelineBarWidth = 18

// timelineFormatDuration renders d for a column a human scans vertically:
// sub-minute values keep a tenth of a second, longer ones drop to whole
// seconds, and the fields are zero-padded so the digits line up.
func timelineFormatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %04.1fs", int(d/time.Minute), (d % time.Minute).Seconds())
	default:
		return fmt.Sprintf("%dh %02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}

// timelineFormatUSD renders one node's cost. A nil Usage means the node was
// never billed at all, which is a different fact from costing nothing, so it
// renders as an absence. An estimated figure is marked, because the ledger
// tracks that distinction precisely and presenting a guess as a fact would
// throw the distinction away.
func timelineFormatUSD(u *journal.Usage) string {
	if u == nil {
		return timelineNoValue
	}
	if u.Estimated {
		return fmt.Sprintf("~$%.3f", u.USD)
	}
	return fmt.Sprintf("$%.3f", u.USD)
}

// timelineEntryLabel names an entry in the node column. A node that ran once
// is just its name; a node that ran several times is numbered, so a reader
// sees "test #2/3" instead of having to notice for themselves that the same
// word appears three times.
func timelineEntryLabel(e TimelineEntry) string {
	if !e.Repeated() {
		return e.Node
	}
	return fmt.Sprintf("%s #%d/%d", e.Node, e.Iteration, e.Iterations)
}

// timelineEntryDuration renders an entry's duration column.
func timelineEntryDuration(e TimelineEntry) string {
	if e.Outcome == TimelineInterrupted {
		return timelineNoValue
	}
	return timelineFormatDuration(e.Duration)
}

// timelineEntryDetail is the short clause appended after a row when the row
// alone would understate what happened.
func timelineEntryDetail(e TimelineEntry) string {
	switch {
	case e.Outcome == TimelineInterrupted:
		return "started, never finished"
	case e.Rerun():
		return fmt.Sprintf("re-ran after an interrupted attempt %d", e.Attempt-1)
	default:
		return ""
	}
}

// timelineInterruptionMessage states, in plain language, that the run stopped
// without recording an ending.
//
// A torn final line is what an append-only journal that fsyncs every record
// looks like from the inside when the process is killed: the newest line is
// the only thing that can be damaged, and everything before it is durable.
// Saying so is the point — a reader who is told their journal is "corrupt"
// learns the wrong lesson about their own data.
func timelineInterruptionMessage(t Timeline) string {
	if !t.Interrupted {
		return ""
	}

	// Where the run stopped depends on what survived. A node left open says
	// exactly which node was running; a torn line with no open node means
	// even that much was lost, and the honest statement is only that nothing
	// was recorded after the last node that did finish.
	where := "here"
	switch {
	case t.InterruptedMidNode():
		where = fmt.Sprintf("while %s was running", t.InterruptedNode)
	case t.InterruptedNode != "":
		where = fmt.Sprintf("after %s finished", t.InterruptedNode)
	}

	if !t.InterruptedMidWrite {
		return fmt.Sprintf("The run was interrupted %s. Everything above was recorded before it stopped.", where)
	}
	return fmt.Sprintf(
		"The run was interrupted %s, and its last journal line was still being written. "+
			"belay flushes every record to disk as it happens, so that unfinished line is the only "+
			"thing lost — everything above it is durable.", where)
}

// timelineWrapWidth is the column the timeline wraps prose at. Node rows are
// never wrapped; only sentences are.
const timelineWrapWidth = 78

// timelineWrap breaks text into lines no wider than width, each prefixed with
// indent. It splits on spaces only, so a single long word overflows rather
// than being cut in half.
func timelineWrap(text, indent string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := make([]string, 0, 4)
	line := indent + words[0]
	for _, w := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > width {
			lines = append(lines, line)
			line = indent + w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}

// timelineStyle applies terminal color, or does nothing at all.
type timelineStyle struct{ enabled bool }

const (
	timelineBold   = "1"
	timelineDim    = "2"
	timelineRed    = "31"
	timelineYellow = "33"
)

// paint wraps text in an SGR sequence when color is on. When it is off — a
// pipe, a file, NO_COLOR — it returns text untouched, so the output a user
// redirects contains no escape sequences at all.
func (s timelineStyle) paint(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// outcome paints an outcome word by how much it should pull the eye.
func (s timelineStyle) outcome(o TimelineOutcome, text string) string {
	switch o {
	case TimelineFailed:
		return s.paint(timelineRed, text)
	case TimelineInterrupted, TimelinePaused, TimelineStopped:
		return s.paint(timelineYellow, text)
	case TimelineFinished:
		return text
	default:
		return text
	}
}

// TimelineColorEnabled reports whether w should receive terminal color.
//
// Color is on only for a character device — an actual terminal — and only
// when NO_COLOR is unset. Per the NO_COLOR convention the variable's mere
// presence disables color regardless of its value, and a TERM of "dumb"
// declares a terminal that cannot render it.
func TimelineColorEnabled(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// timelineColumns are the measured widths of the table's fixed columns.
type timelineColumns struct {
	index, node, outcome, duration, cost int
	maxDuration                          time.Duration
}

// timelineMeasure sizes every column to its widest cell, so nothing wraps and
// the numeric columns can be right-aligned into scannable stacks.
func timelineMeasure(entries []TimelineEntry) timelineColumns {
	c := timelineColumns{
		index: len("#"), node: len("node"), outcome: len("outcome"),
		duration: len("duration"), cost: len("cost"),
	}
	for _, e := range entries {
		c.index = max(c.index, len(strconv.Itoa(e.Index)))
		c.node = max(c.node, utf8.RuneCountInString(timelineEntryLabel(e)))
		c.outcome = max(c.outcome, len(string(e.Outcome)))
		c.duration = max(c.duration, len(timelineEntryDuration(e)))
		c.cost = max(c.cost, len(timelineFormatUSD(e.Usage)))
		if e.Duration > c.maxDuration {
			c.maxDuration = e.Duration
		}
	}
	return c
}

// timelineBar draws a proportional duration bar.
//
// The bar is plain text — block characters, never an escape sequence — so it
// survives being piped to a file or read in a pager; only its color is
// dropped when the destination is not a terminal. Any non-zero duration gets
// at least one block, so a fast node is visibly present rather than blank.
func timelineBar(d, longest time.Duration) string {
	if d <= 0 || longest <= 0 {
		return ""
	}
	width := int(math.Round(float64(timelineBarWidth) * float64(d) / float64(longest)))
	return strings.Repeat("█", max(width, 1))
}

// timelinePadRight pads s to width columns, counting runes rather than bytes.
func timelinePadRight(s string, width int) string {
	if pad := width - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// timelinePadLeft right-aligns s in width columns, counting runes rather than bytes.
func timelinePadLeft(s string, width int) string {
	if pad := width - utf8.RuneCountInString(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// RenderTimeline writes the human-readable timeline to w.
//
// color must be false whenever w is not a terminal; see TimelineColorEnabled.
// With color off the output contains no escape sequences of any kind, so a
// redirected timeline is clean text.
func RenderTimeline(w io.Writer, tl Timeline, color bool) error {
	s := timelineStyle{enabled: color}
	var b strings.Builder

	timelineWriteHeader(&b, s, tl)
	if len(tl.Entries) == 0 {
		b.WriteString("\nThis run has not recorded any nodes yet.\n")
		for _, line := range timelineWrap(timelineInterruptionMessage(tl), "", timelineWrapWidth) {
			fmt.Fprintf(&b, "%s\n", line)
		}
		_, err := io.WriteString(w, b.String())
		return err
	}
	timelineWriteTable(&b, s, tl)
	timelineWriteSummary(&b, s, tl)

	_, err := io.WriteString(w, b.String())
	return err
}

// timelineWriteHeader writes the identity block: which run this is, how it
// was chosen, what it was for, and where it stands.
func timelineWriteHeader(b *strings.Builder, s timelineStyle, tl Timeline) {
	label := func(k string) string { return s.paint(timelineDim, timelinePadRight(k, 8)) }

	fmt.Fprintf(b, "%s%s\n", label("run"), s.paint(timelineBold, tl.Run.RunID))
	if tl.Run.Implicit {
		// Never act on an implicit target without saying which one was
		// chosen, and say what it was chosen among.
		switch tl.Run.TotalRuns {
		case 1:
			fmt.Fprintf(b, "%s%s\n", label(""), s.paint(timelineDim, "the only run in this workspace"))
		default:
			fmt.Fprintf(b, "%s%s\n", label(""), s.paint(timelineDim, fmt.Sprintf(
				"most recent of %d runs — `belay runs` lists the rest", tl.Run.TotalRuns)))
		}
	}
	if tl.Goal != "" {
		fmt.Fprintf(b, "%s%s\n", label("goal"), tl.Goal)
	}

	status := tl.Status
	if tl.Next != "" && (tl.Interrupted || tl.Status == "unfinished" || strings.HasPrefix(tl.Status, "paused")) {
		status = fmt.Sprintf("%s — %s would run next", status, tl.Next)
		if tl.NextAttempt > 1 {
			status = fmt.Sprintf("%s (attempt %d)", status, tl.NextAttempt)
		}
	}
	fmt.Fprintf(b, "%s%s\n", label("status"), status)
}

// timelineWriteTable writes the ordered node rows, with the pause and resume
// markers interleaved where they happened.
func timelineWriteTable(b *strings.Builder, s timelineStyle, tl Timeline) {
	c := timelineMeasure(tl.Entries)
	gutter := strings.Repeat(" ", c.index+4)

	b.WriteString("\n")
	fmt.Fprintf(b, "  %s  %s  %s  %s  %s\n",
		s.paint(timelineDim, timelinePadLeft("#", c.index)),
		s.paint(timelineDim, timelinePadRight("node", c.node)),
		s.paint(timelineDim, timelinePadRight("outcome", c.outcome)),
		s.paint(timelineDim, timelinePadLeft("duration", c.duration)),
		s.paint(timelineDim, timelinePadLeft("cost", c.cost)))

	timelineWriteMarkers(b, s, tl, gutter, 0)
	for _, e := range tl.Entries {
		// Trimmed rather than padded to a fixed width: a row whose bar is
		// empty must not leave trailing whitespace behind in a file.
		row := fmt.Sprintf("  %s  %s  %s  %s  %s  %s",
			timelinePadLeft(strconv.Itoa(e.Index), c.index),
			timelinePadRight(timelineEntryLabel(e), c.node),
			s.outcome(e.Outcome, timelinePadRight(string(e.Outcome), c.outcome)),
			timelinePadLeft(timelineEntryDuration(e), c.duration),
			timelinePadLeft(timelineFormatUSD(e.Usage), c.cost),
			s.paint(timelineDim, timelineBar(e.Duration, c.maxDuration)))
		b.WriteString(strings.TrimRight(row, " ") + "\n")

		if detail := timelineEntryDetail(e); detail != "" {
			fmt.Fprintf(b, "%s%s\n", gutter, s.paint(timelineDim, detail))
		}
		if e.Note != "" {
			fmt.Fprintf(b, "%s%s\n", gutter, s.paint(timelineDim, e.Note))
		}
		timelineWriteMarkers(b, s, tl, gutter, e.Index)
	}

	if msg := timelineInterruptionMessage(tl); msg != "" {
		b.WriteString("\n")
		for _, line := range timelineWrap(msg, "  ", timelineWrapWidth) {
			fmt.Fprintf(b, "%s\n", line)
		}
	}
}

// timelineWriteMarkers writes the run-level markers recorded after the entry
// at afterIndex. A pause immediately followed by its resume is written as one
// sentence, so the reader is told how long the wait was instead of being left
// to subtract two timestamps.
func timelineWriteMarkers(b *strings.Builder, s timelineStyle, tl Timeline, gutter string, afterIndex int) {
	for i := 0; i < len(tl.Markers); i++ {
		m := tl.Markers[i]
		if m.AfterIndex != afterIndex {
			continue
		}
		if m.Kind == "paused" && i+1 < len(tl.Markers) &&
			tl.Markers[i+1].Kind == "resumed" && tl.Markers[i+1].AfterIndex == afterIndex {
			fmt.Fprintf(b, "%s%s\n", gutter, s.paint(timelineDim, fmt.Sprintf(
				"waited %s here, then resumed", timelineFormatDuration(tl.Markers[i+1].Gap))))
			i++
			continue
		}
		fmt.Fprintf(b, "%s%s\n", gutter, s.paint(timelineDim, "run "+m.Kind+" here"))
	}
}

// timelineWriteSummary writes the totals block: every number the reader would
// otherwise have to add up themselves.
func timelineWriteSummary(b *strings.Builder, s timelineStyle, tl Timeline) {
	label := func(k string) string { return "  " + s.paint(timelineDim, timelinePadRight(k, 12)) }
	b.WriteString("\n")

	if len(tl.Loops) > 0 {
		parts := make([]string, 0, len(tl.Loops))
		for _, l := range tl.Loops {
			parts = append(parts, fmt.Sprintf("%s ran %d times", l.Node, l.Executions))
		}
		fmt.Fprintf(b, "%s%s\n", label("loops"), strings.Join(parts, ", "))

		share := ""
		if tl.Totals.USD > 0 {
			share = fmt.Sprintf(" — %.0f%% of the run's spend", 100*tl.Totals.LoopUSD/tl.Totals.USD)
		}
		// Naming the rows the figure came from lets the reader check it
		// against the table instead of taking it on faith.
		fmt.Fprintf(b, "%ssteps %d-%d cost $%.3f and %s%s\n", label("loop cost"),
			tl.Totals.LoopFirstIndex, tl.Totals.LoopLastIndex,
			tl.Totals.LoopUSD, timelineFormatDuration(tl.Totals.LoopDuration), share)
	}

	fmt.Fprintf(b, "%s%s across %d nodes\n", label("agent time"),
		timelineFormatDuration(tl.Totals.AgentDuration), tl.Totals.Nodes)
	fmt.Fprintf(b, "%s%s%s\n", label("wall clock"),
		timelineFormatDuration(tl.Totals.WallDuration), timelineSpan(tl))

	spend := fmt.Sprintf("$%.3f", tl.Totals.USD)
	if tl.Totals.BudgetLimitUSD > 0 {
		spend += fmt.Sprintf(" of the $%.2f ceiling", tl.Totals.BudgetLimitUSD)
	}
	fmt.Fprintf(b, "%s%s\n", label("spend"), spend)
	if tl.Totals.Estimated {
		// An estimate presented as a fact would throw away a distinction the
		// budget ledger tracks precisely, so it gets its own line rather than
		// a parenthetical someone can skim past.
		fmt.Fprintf(b, "%s%s\n", label(""),
			s.paint(timelineDim, "estimated — at least one node's cost was a heuristic, not a reported charge"))
	}
}

// timelineSpan renders the clock times the wall duration spans, so a reader
// can line the run up against anything else that happened that day.
func timelineSpan(tl Timeline) string {
	if tl.StartedAt.IsZero() || tl.EndedAt.IsZero() {
		return ""
	}
	start, end := tl.StartedAt.UTC(), tl.EndedAt.UTC()
	if start.Format("2006-01-02") == end.Format("2006-01-02") {
		return fmt.Sprintf("   %s → %s", start.Format("2006-01-02 15:04:05Z"), end.Format("15:04:05Z"))
	}
	return fmt.Sprintf("   %s → %s",
		start.Format("2006-01-02 15:04:05Z"), end.Format("2006-01-02 15:04:05Z"))
}

// TimelineJSONVersion is the schema version of the `--json` document. It is
// part of the command's contract: fields are added within a version, and any
// removal or change of meaning increments it, so a script can key off it.
const TimelineJSONVersion = 1

// timelineJSONDoc is the stable `--json` shape.
//
//	{
//	  "schema": 1,
//	  "run_id":       string,      // the run this timeline describes
//	  "run_dir":      string,      // absolute path to the run directory
//	  "workspace":    string,      // absolute path to the repository
//	  "selected":     string,      // "explicit" | "most-recent"
//	  "total_runs":   number,      // runs present in the workspace
//	  "goal":         string,      // "" when no manifest was readable
//	  "status":       string,      // finished|failed|paused…|stopped|unfinished|interrupted
//	  "next":         string|null, // node that would run next
//	  "next_attempt": number|null,
//	  "started_at":   string|null, // RFC3339
//	  "ended_at":     string|null,
//	  "interrupted":  bool,
//	  "interruption": null | {"node": string, "mid_write": bool, "message": string},
//	  "entries":      [ …entry ],  // always present, possibly empty
//	  "markers":      [ …marker ],
//	  "loops":        [ {"node": string, "executions": number} ],
//	  "totals":       { …totals }
//	}
//
// An entry is one node execution:
//
//	{
//	  "index": number, "node": string,
//	  "iteration": number, "iterations": number,   // "test #2/3"
//	  "attempt": number, "rerun_after_interruption": bool,
//	  "outcome": "finished"|"failed"|"paused"|"stopped"|"interrupted",
//	  "started_at": string|null, "finished_at": string|null,
//	  "duration_ms": number|null,                  // null when it never finished
//	  "cost": null | {"usd": number, "estimated": bool,
//	                  "tokens_in": number, "tokens_out": number},
//	  "next": string, "note": string
//	}
//
// A null cost means the node was never billed; a cost object with "usd": 0
// means it was billed and was free. The two are not interchangeable.
type timelineJSONDoc struct {
	Schema       int                       `json:"schema"`
	RunID        string                    `json:"run_id"`
	RunDir       string                    `json:"run_dir"`
	Workspace    string                    `json:"workspace"`
	Selected     string                    `json:"selected"`
	TotalRuns    int                       `json:"total_runs"`
	Goal         string                    `json:"goal"`
	Status       string                    `json:"status"`
	Next         *string                   `json:"next"`
	NextAttempt  *int                      `json:"next_attempt"`
	StartedAt    *time.Time                `json:"started_at"`
	EndedAt      *time.Time                `json:"ended_at"`
	Interrupted  bool                      `json:"interrupted"`
	Interruption *timelineJSONInterruption `json:"interruption"`
	Entries      []timelineJSONEntry       `json:"entries"`
	Markers      []timelineJSONMarker      `json:"markers"`
	Loops        []timelineJSONLoop        `json:"loops"`
	Totals       timelineJSONTotals        `json:"totals"`
}

// timelineJSONInterruption reports a run that stopped without recording an
// ending, carrying the same plain sentence the text view prints.
type timelineJSONInterruption struct {
	Node     string `json:"node"`
	MidWrite bool   `json:"mid_write"`
	Message  string `json:"message"`
}

// timelineJSONEntry is one node execution.
type timelineJSONEntry struct {
	Index      int               `json:"index"`
	Node       string            `json:"node"`
	Iteration  int               `json:"iteration"`
	Iterations int               `json:"iterations"`
	Attempt    int               `json:"attempt"`
	Rerun      bool              `json:"rerun_after_interruption"`
	Outcome    TimelineOutcome   `json:"outcome"`
	StartedAt  *time.Time        `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at"`
	DurationMS *int64            `json:"duration_ms"`
	Cost       *timelineJSONCost `json:"cost"`
	Next       string            `json:"next"`
	Note       string            `json:"note"`
}

// timelineJSONCost is one node's accounted spend.
type timelineJSONCost struct {
	USD       float64 `json:"usd"`
	Estimated bool    `json:"estimated"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
}

// timelineJSONMarker is a run-level pause or resume between two entries.
type timelineJSONMarker struct {
	AfterIndex int       `json:"after_index"`
	Kind       string    `json:"kind"`
	Time       time.Time `json:"time"`
	GapMS      int64     `json:"gap_ms"`
}

// timelineJSONLoop names a node that ran more than once.
type timelineJSONLoop struct {
	Node       string `json:"node"`
	Executions int    `json:"executions"`
}

// timelineJSONTotals is the whole-run arithmetic.
type timelineJSONTotals struct {
	Nodes           int      `json:"nodes"`
	AgentDurationMS int64    `json:"agent_duration_ms"`
	WallDurationMS  int64    `json:"wall_duration_ms"`
	USD             float64  `json:"usd"`
	Estimated       bool     `json:"estimated"`
	TokensIn        int64    `json:"tokens_in"`
	TokensOut       int64    `json:"tokens_out"`
	BudgetLimitUSD  *float64 `json:"budget_limit_usd"`
	LoopUSD         float64  `json:"loop_usd"`
	LoopDurationMS  int64    `json:"loop_duration_ms"`
	LoopFirstIndex  int      `json:"loop_first_index"`
	LoopLastIndex   int      `json:"loop_last_index"`
}

// timelineTimePtr converts a time to a pointer, mapping the zero time to nil
// so an unrecorded timestamp encodes as null rather than as year 1.
func timelineTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// timelineToJSON projects a Timeline onto the pinned wire shape. The
// projection exists so the in-memory model can change without silently
// changing what scripts parse.
func timelineToJSON(tl Timeline) timelineJSONDoc {
	selected := "explicit"
	if tl.Run.Implicit {
		selected = "most-recent"
	}
	doc := timelineJSONDoc{
		Schema:      TimelineJSONVersion,
		RunID:       tl.Run.RunID,
		RunDir:      tl.Run.Dir,
		Workspace:   tl.Run.Workspace,
		Selected:    selected,
		TotalRuns:   tl.Run.TotalRuns,
		Goal:        tl.Goal,
		Status:      tl.Status,
		StartedAt:   timelineTimePtr(tl.StartedAt),
		EndedAt:     timelineTimePtr(tl.EndedAt),
		Interrupted: tl.Interrupted,
		// Collections are always present as arrays, never null, so a
		// consumer can iterate without a nil check.
		Entries: make([]timelineJSONEntry, 0, len(tl.Entries)),
		Markers: make([]timelineJSONMarker, 0, len(tl.Markers)),
		Loops:   make([]timelineJSONLoop, 0, len(tl.Loops)),
		Totals: timelineJSONTotals{
			Nodes:           tl.Totals.Nodes,
			AgentDurationMS: tl.Totals.AgentDuration.Milliseconds(),
			WallDurationMS:  tl.Totals.WallDuration.Milliseconds(),
			USD:             tl.Totals.USD,
			Estimated:       tl.Totals.Estimated,
			TokensIn:        tl.Totals.TokensIn,
			TokensOut:       tl.Totals.TokensOut,
			LoopUSD:         tl.Totals.LoopUSD,
			LoopDurationMS:  tl.Totals.LoopDuration.Milliseconds(),
			LoopFirstIndex:  tl.Totals.LoopFirstIndex,
			LoopLastIndex:   tl.Totals.LoopLastIndex,
		},
	}
	if tl.Next != "" {
		next, attempt := tl.Next, tl.NextAttempt
		doc.Next, doc.NextAttempt = &next, &attempt
	}
	if tl.Totals.BudgetLimitUSD > 0 {
		limit := tl.Totals.BudgetLimitUSD
		doc.Totals.BudgetLimitUSD = &limit
	}
	if tl.Interrupted {
		doc.Interruption = &timelineJSONInterruption{
			Node:     tl.InterruptedNode,
			MidWrite: tl.InterruptedMidWrite,
			Message:  timelineInterruptionMessage(tl),
		}
	}

	for _, e := range tl.Entries {
		je := timelineJSONEntry{
			Index: e.Index, Node: e.Node,
			Iteration: e.Iteration, Iterations: e.Iterations,
			Attempt: e.Attempt, Rerun: e.Rerun(), Outcome: e.Outcome,
			StartedAt: timelineTimePtr(e.StartedAt), FinishedAt: timelineTimePtr(e.FinishedAt),
			Next: e.Next, Note: e.Note,
		}
		if e.Outcome != TimelineInterrupted {
			ms := e.Duration.Milliseconds()
			je.DurationMS = &ms
		}
		if u := e.Usage; u != nil {
			je.Cost = &timelineJSONCost{
				USD: u.USD, Estimated: u.Estimated,
				TokensIn: u.InputTokens, TokensOut: u.OutputTokens,
			}
		}
		doc.Entries = append(doc.Entries, je)
	}
	for _, m := range tl.Markers {
		doc.Markers = append(doc.Markers, timelineJSONMarker{
			AfterIndex: m.AfterIndex, Kind: m.Kind, Time: m.Time.UTC(), GapMS: m.Gap.Milliseconds(),
		})
	}
	for _, l := range tl.Loops {
		doc.Loops = append(doc.Loops, timelineJSONLoop(l))
	}
	return doc
}

// RenderTimelineJSON writes tl to w as the documented, versioned JSON
// document — the scriptable form of exactly what the text view shows.
func RenderTimelineJSON(w io.Writer, tl Timeline) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(timelineToJSON(tl)); err != nil {
		return fmt.Errorf("cli: encode timeline as json: %w", err)
	}
	return nil
}

func init() {
	// Subcommands attach themselves from their own file, so root.go never
	// needs editing to add one.
	rootCmd.AddCommand(newTimelineCmd())
}

// newTimelineCmd builds the `belay timeline` command.
func newTimelineCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "timeline [run-id]",
		Short: "Show one run's execution order, timing and cost",
		Long: `Show how one run actually unfolded: every node in the order it ran, how
long each took, what each cost, and where the run looped.

With no run id, the most recent run in the workspace is used and named in the
output. A node that ran more than once is numbered — "test #2/3" — so a fix
loop reads as a loop rather than as a list you have to count.

A run that was interrupted has a journal whose final line was still being
written. That is expected: belay flushes every record to disk as it happens,
so only the unfinished line is ever lost. The timeline renders everything
before it and says where the run stopped.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := workspaceDirOf(cmd)
			if err != nil {
				return err
			}
			var runID string
			if len(args) == 1 {
				runID = args[0]
			}
			tl, err := LoadTimeline(ws, runID)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return RenderTimelineJSON(out, tl)
			}
			return RenderTimeline(out, tl, TimelineColorEnabled(out))
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"emit the timeline as JSON (stable, versioned shape) instead of a table")
	return cmd
}
