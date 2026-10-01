//go:build fixrate

package fixrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file is the corpus runner: both arms, over a whole corpus of tasks,
// scored by the official harness, in a run that survives being interrupted.
//
// # The unit of work is a cell
//
// A cell is one task, in one arm, at one replicate: "django__django-11099,
// belay arm, first run". A full SWE-bench Verified run is 500 tasks in two
// arms, about a thousand cells, and at minutes of agent time each it takes
// days. It will be interrupted -- by a person, a reboot, a dropped network,
// a kill -9 -- and the owner's rule for this harness is the same one belay
// itself is built on: a resumed run must never run again, and never pay
// again for, a cell that finished.
//
// # How a resumed run knows what finished
//
// Every cell has a directory, cells/<task>/<arm>-r<n>/, and three files in
// it settle what is true about it, each written atomically (atomicfile.go):
//
//   - result.json exists only once the cell's work is complete and its patch
//     is taken. Its presence is the whole of "this cell finished": a resumed
//     run skips the cell, and nothing ever rewrites the file.
//   - verdict.json exists only once the official harness has scored that
//     patch. Its absence is the whole of "still to be scored".
//   - attempts.json counts the attempts the cell has started, and says
//     whether one is still open -- which, found at the start of the next
//     attempt, means the last one died without closing.
//
// Between "started" and "result.json", what a resumed run does depends on
// the arm, because the arms are not alike:
//
//   - The belay arm keeps belay's own journal, beside the cell at
//     .belay/runs/belay/ -- outside the workspace, so it is never part of a
//     patch. An interrupted belay cell is resumed, not restarted: the
//     dispatcher re-runs only the step that was cut off, and every step that
//     finished is neither run nor paid for again. That is belay's whole
//     thesis, and its benchmark relies on it rather than reimplementing it.
//   - The control arm is one call, and a call cannot be resumed. A control
//     cell cut off mid-call runs again, against a fresh workspace, so the
//     second call starts where every control call starts and not from half
//     of the first one's edit. But the moment a call returns, its response
//     is kept (response.json) before anything else is done with it, so an
//     interruption after the call -- while the patch is being taken, say --
//     never pays for the call again.
//
// # The spend cap is checked before spending, never after
//
// A live corpus has a total cap (CorpusOptions.MaxUSD). Before each cell
// starts, the corpus adds everything spent so far to the most the cell may
// spend, and starts the cell only if that fits. "The most a cell may spend"
// is belay's budget.max_usd: the belay arm's own dispatcher enforces it per
// run, and the control arm's one call is reserved the same amount, because
// belay can bound a call's turns and time but not its dollars. The cap can
// therefore be overrun only by what one cell spends past its own reservation
// -- a belay run's last step, or a control call -- and never by a cell the
// corpus started knowing it could not afford.
//
// What a cut-off attempt spent is unknowable: the process that would have
// reported it was stopped. The corpus charges such an attempt the cell's
// whole reservation, as an estimate, the way belay charges a step a crash
// left open its run's costliest step (ADR-0007). Charging too much can only
// stop a corpus early; charging nothing is how a cap gets overrun. The
// charge counts toward the cap and is reported apart from what the arms
// spent, so a reader can see it; it is not part of an arm's cost per
// resolved task, which is what the arm itself reported.
//
// # A corpus is one measurement
//
// The first run pins what the corpus is (corpus.json): its tasks, arms and
// replicates, the records' sha256, the mode, and the digest of the belay
// configuration both arms run under. A resume that asks for anything else
// is refused by name, before anything runs, because a corpus whose cells ran
// under two configurations is two measurements quoted as one. The spend cap
// is deliberately not pinned: raising it and resuming is how a corpus that
// stopped at its cap continues, the way raising budget.max_usd and resuming
// continues a belay run.
//
// # What it does not do
//
// It runs one cell at a time. A full run is therefore slow, days rather than
// hours; the cells are independent, and running several at once is the
// obvious next step, but every guarantee above was written for one at a
// time and is tested that way. It builds no workspace itself -- that is the
// Provisioner's, which for SWE-bench is the instance image (container.go) --
// and it constructs no agent: AgentFor is the caller's, so a live corpus is
// the caller's decision to spend, exactly as with Run.

// Errors RunCorpus returns. Each is matchable with errors.Is.
var (
	// ErrNoSpendCap reports a live corpus with no total spend cap.
	ErrNoSpendCap = errors.New("fixrate: a live corpus needs a total spend cap (CorpusOptions.MaxUSD)")
	// ErrSpendCapReached reports a corpus that stopped because the next cell
	// could have taken it past its cap. It is resumable: raise the cap.
	ErrSpendCapReached = errors.New("fixrate: the corpus stopped at its spend cap")
	// ErrCorpusInterrupted reports a corpus whose context was cancelled. It
	// is resumable: run it again with the same options.
	ErrCorpusInterrupted = errors.New("fixrate: the corpus was interrupted; running it again resumes it")
	// ErrCorpusMismatch reports a corpus directory that holds a corpus
	// pinned with different settings from the ones a resume asked for.
	ErrCorpusMismatch = errors.New("fixrate: the corpus directory holds a different corpus")
	// errCorpusConfig reports options RunCorpus cannot run with. It is a
	// wiring bug, never an outcome.
	errCorpusConfig = errors.New("fixrate: unusable corpus configuration")
	// ErrInfrastructure reports a cell that failed for want of what the
	// harness provides -- a test runner or a lint gate that could not run,
	// or, in a live corpus, a backend that failed without spending anything
	// -- rather than for anything its arm did. Nothing is recorded for the
	// cell, and it is resumable once the cause is mended. See cell.go,
	// "When the harness fails, not the arm".
	ErrInfrastructure = errors.New("fixrate: the harness's infrastructure failed")
)

// The statuses a CorpusReport can have.
const (
	// CorpusComplete means every cell finished and was scored.
	CorpusComplete = "complete"
	// CorpusStoppedAtCap means the corpus stopped before a cell that could
	// have taken it past its spend cap.
	CorpusStoppedAtCap = "stopped at the spend cap"
	// CorpusInterrupted means the corpus was interrupted.
	CorpusInterrupted = "interrupted"
	// CorpusStopped means the corpus stopped on an error; the report says
	// which in its warnings.
	CorpusStopped = "stopped on an error"
)

// corpusSchemaVersion versions corpus.json and every cell file.
const corpusSchemaVersion = 1

// belayCellRunID names the belay run inside a belay cell's directory. Each
// cell has its own directory, so one fixed name never collides.
const belayCellRunID = "belay"

// Cell is one unit of corpus work: one task, in one arm, at one replicate.
type Cell struct {
	Task      Task
	Arm       Arm
	Replicate int
}

// System is the name the cell's prediction is scored under: its arm and
// replicate, such as "belay-r1". The official harness caches verdicts by
// run id, model name and instance, so every arm and replicate needs its own.
func (c Cell) System() string { return fmt.Sprintf("%s-r%d", c.Arm, c.Replicate) }

// Key names the cell within its corpus, "<task>/<system>", which is also its
// directory under cells/.
func (c Cell) Key() string { return c.Task.ID + "/" + c.System() }

// Workspace is the workspace one cell's arm works in. It is plain data
// together with the two adapters the belay arm's graph runs in it.
type Workspace struct {
	// Dir is where the agent edits: an absolute path.
	Dir string
	// Base is the commit the cell's change is measured from, the one its
	// patch applies to.
	Base string
	// Runner is the belay arm's test node's runner for this workspace.
	Runner belay.TestRunner
	// Linter is the belay arm's quality gate for this workspace.
	Linter belay.Linter
	// Adapters says what Runner and Linter are, in words, so every result
	// the belay arm produces says which parts of the graph were supplied by
	// the harness rather than shipped with belay.
	Adapters string
}

// Provisioner sets up the workspace a cell works in, and takes it down.
type Provisioner interface {
	// Provision returns the cell's workspace, under the cell's directory
	// dir. With fresh true, any workspace an earlier attempt left there is
	// discarded first and the cell starts from the task's base. With fresh
	// false, a workspace an earlier attempt left is returned exactly as it
	// was left -- a resumed belay run depends on finding its own edits.
	Provision(ctx context.Context, cell Cell, dir string, fresh bool) (Workspace, error)
	// Release frees whatever Provision started for the cell and removes its
	// workspace. It must tolerate a workspace already released.
	Release(ctx context.Context, cell Cell, dir string) error
}

// Evaluator scores one cell's patch. Harness is the official one.
type Evaluator interface {
	Evaluate(ctx context.Context, task Task, system, patch string) (Verdict, error)
}

var _ Evaluator = Harness{}

// CorpusOptions configures RunCorpus. A resumed corpus is given the same
// options again; see this file's "A corpus is one measurement".
type CorpusOptions struct {
	// Mode is ModeReplay or ModeLive, as for Run.
	Mode Mode
	// Confirm must equal ConfirmLiveSpend for ModeLive.
	Confirm string
	// Dir is the corpus directory, absolute: created on the first run,
	// resumed on every later one.
	Dir string
	// RunID names the corpus's evaluation run in the official harness.
	RunID string
	// Corpus labels the records the tasks came from, "dataset@revision".
	Corpus string
	// RecordsSHA256 is the sha256 of the records file the tasks were read
	// from, and that the evaluator grades against.
	RecordsSHA256 string
	// Selection says in words how the tasks were chosen.
	Selection string
	// Tasks are the corpus, in the order it runs.
	Tasks []Task
	// Arms are the arms each task runs in; nil means both.
	Arms []Arm
	// Replicates is how many times each arm runs each task: 1, or 2 for an
	// A/A replicate.
	Replicates int
	// Config is the belay configuration both arms run under. Its
	// graph.approval is turned off, because nobody is there to approve.
	Config config.Config
	// AgentFor returns the backend a cell's arm drives. The corpus never
	// constructs one; see Mode.
	AgentFor func(Cell) belay.AgentBackend
	// Provisioner sets up each cell's workspace.
	Provisioner Provisioner
	// Evaluator scores each cell's patch.
	Evaluator Evaluator
	// MaxUSD is the corpus's total spend cap. Required in ModeLive. It is
	// read on every run rather than pinned, so a corpus that stopped at it
	// can be resumed with a higher one.
	MaxUSD float64
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
	// Logger receives progress. Defaults to a discarding logger.
	Logger *slog.Logger
}

// CorpusManifest is what the first run of a corpus pins in corpus.json.
type CorpusManifest struct {
	SchemaVersion int       `json:"schema_version"`
	RunID         string    `json:"run_id"`
	Mode          Mode      `json:"mode"`
	Corpus        string    `json:"corpus"`
	RecordsSHA256 string    `json:"records_sha256"`
	Selection     string    `json:"selection"`
	Tasks         []string  `json:"tasks"`
	Arms          []Arm     `json:"arms"`
	Replicates    int       `json:"replicates"`
	ConfigDigest  string    `json:"config_digest"`
	Model         string    `json:"model"`
	CreatedAt     time.Time `json:"created_at"`
}

// corpusRun is one invocation of RunCorpus.
type corpusRun struct {
	opts     CorpusOptions
	cfg      config.Config
	arms     []Arm
	now      func() time.Time
	logger   *slog.Logger
	manifest CorpusManifest
	ledger   corpusLedger
}

// RunCorpus runs, or resumes, a corpus: every task in every arm and
// replicate, each cell's patch scored by opts.Evaluator, and the aggregate
// report written to report.json and report.txt in the corpus directory and
// returned. See this file's header comment for what a resumed run does.
//
// It stops early, resumably, in three ways, each returning the report so
// far together with an error that says why: ErrCorpusInterrupted when ctx is
// cancelled, ErrSpendCapReached before a cell that could pass the cap, and
// any error from a workspace, a git command or the evaluator that leaves a
// cell without a result or a verdict. An invalid opts is refused before
// anything is written, with a nil report.
func RunCorpus(ctx context.Context, opts CorpusOptions) (*CorpusReport, error) {
	r, err := newCorpusRun(opts)
	if err != nil {
		return nil, err
	}
	if err := r.pin(); err != nil {
		return nil, err
	}
	if err := r.loadLedger(); err != nil {
		return nil, err
	}

	stop := r.work(ctx)
	status := CorpusComplete
	switch {
	case stop == nil:
	case errors.Is(stop, ErrCorpusInterrupted):
		status = CorpusInterrupted
	case errors.Is(stop, ErrSpendCapReached):
		status = CorpusStoppedAtCap
	default:
		status = CorpusStopped
	}
	report, err := r.report(status, stop)
	if err != nil {
		return nil, errors.Join(stop, err)
	}
	return report, stop
}

// newCorpusRun validates opts and settles what this run works with.
func newCorpusRun(opts CorpusOptions) (*corpusRun, error) {
	if !opts.Mode.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, opts.Mode)
	}
	if opts.Mode == ModeLive {
		if opts.Confirm != ConfirmLiveSpend {
			return nil, ErrLiveConfirmationRequired
		}
		if opts.MaxUSD <= 0 {
			return nil, ErrNoSpendCap
		}
	}
	if err := validateCorpusOptions(opts); err != nil {
		return nil, err
	}

	arms := opts.Arms
	if arms == nil {
		arms = []Arm{ArmBelay, ArmControl}
	}
	cfg := opts.Config
	cfg.Graph.Approval = false

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	reservation := 0.0
	if opts.Mode == ModeLive {
		reservation = cfg.Budget.MaxUSD
	}
	tasks := make([]string, 0, len(opts.Tasks))
	for _, t := range opts.Tasks {
		tasks = append(tasks, t.ID)
	}
	return &corpusRun{
		opts:   opts,
		cfg:    cfg,
		arms:   arms,
		now:    now,
		logger: logger,
		manifest: CorpusManifest{
			SchemaVersion: corpusSchemaVersion,
			RunID:         opts.RunID,
			Mode:          opts.Mode,
			Corpus:        opts.Corpus,
			RecordsSHA256: opts.RecordsSHA256,
			Selection:     opts.Selection,
			Tasks:         tasks,
			Arms:          arms,
			Replicates:    opts.Replicates,
			ConfigDigest:  cfg.Digest(),
			Model:         cfg.Agent.ModelFor("code"),
			CreatedAt:     now().UTC(),
		},
		ledger: corpusLedger{cap: opts.MaxUSD, reservation: reservation},
	}, nil
}

// validateCorpusOptions refuses options RunCorpus cannot honour.
func validateCorpusOptions(opts CorpusOptions) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", errCorpusConfig, fmt.Sprintf(format, args...))
	}
	switch {
	case !filepath.IsAbs(opts.Dir):
		return bad("the corpus directory %q is not an absolute path", opts.Dir)
	case !pathSegment.MatchString(opts.RunID):
		return bad("run id %q", opts.RunID)
	case len(opts.Tasks) == 0:
		return bad("no tasks")
	case opts.Replicates != 1 && opts.Replicates != 2:
		return bad("%d replicate(s); a corpus runs each arm once, or twice for an A/A replicate", opts.Replicates)
	case opts.AgentFor == nil:
		return bad("no agent")
	case opts.Provisioner == nil:
		return bad("no provisioner")
	case opts.Evaluator == nil:
		return bad("no evaluator")
	case opts.MaxUSD < 0:
		return bad("a negative spend cap")
	case opts.Config.Budget.MaxUSD <= 0:
		return bad("budget.max_usd is %v; the corpus reserves it for every cell before starting it, so it must be set", opts.Config.Budget.MaxUSD)
	case opts.Config.Fanout.Enabled:
		return bad("fanout is enabled; the corpus measures belay's single-run graph")
	case opts.Config.Review.Mode != config.ReviewModeLint:
		return bad("review.mode is %q; the corpus supplies the lint gate each workspace needs, and no other", opts.Config.Review.Mode)
	case !reflect.ValueOf(opts.Config.Policy).IsZero():
		return bad("a policy is configured; the corpus's graph has no policy gate, so it would not be enforced")
	}
	seen := make(map[string]bool, len(opts.Tasks))
	for _, t := range opts.Tasks {
		if !pathSegment.MatchString(t.ID) {
			return bad("task id %q cannot name a directory", t.ID)
		}
		if seen[t.ID] {
			return bad("task %q appears twice", t.ID)
		}
		seen[t.ID] = true
	}
	var arms []Arm
	for _, a := range opts.Arms {
		if a != ArmBelay && a != ArmControl {
			return bad("unknown arm %q", a)
		}
		if slices.Contains(arms, a) {
			return bad("arm %q appears twice", a)
		}
		arms = append(arms, a)
	}
	if opts.Arms != nil && len(arms) == 0 {
		return bad("no arms")
	}
	return nil
}

// pin creates the corpus directory and its corpus.json on a first run, and
// on any later run refuses settings that differ from the pinned ones.
func (r *corpusRun) pin() error {
	path := filepath.Join(r.opts.Dir, "corpus.json")
	data, err := os.ReadFile(path) //nolint:gosec // the corpus's own file
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Join(r.opts.Dir, "cells"), 0o750); err != nil {
			return fmt.Errorf("fixrate: create the corpus directory: %w", err)
		}
		return writeJSONAtomic(path, r.manifest)
	case err != nil:
		return fmt.Errorf("fixrate: read %s: %w", path, err)
	}
	var pinned CorpusManifest
	if err := json.Unmarshal(data, &pinned); err != nil {
		return fmt.Errorf("fixrate: %s is not a corpus manifest: %w", path, err)
	}
	if err := r.manifest.matches(pinned); err != nil {
		return fmt.Errorf("%w in %s: %w", ErrCorpusMismatch, r.opts.Dir, err)
	}
	r.manifest = pinned
	return nil
}

// matches reports the first setting want pins differently from m, by name.
func (m CorpusManifest) matches(pinned CorpusManifest) error {
	differ := func(field string, was, now any) error {
		return fmt.Errorf("it was pinned with %s %v, and this run asks for %v; "+
			"resume it with the settings it started with, or start a new corpus in another directory", field, was, now)
	}
	switch {
	case pinned.SchemaVersion != m.SchemaVersion:
		return differ("schema version", pinned.SchemaVersion, m.SchemaVersion)
	case pinned.Mode != m.Mode:
		return differ("mode", pinned.Mode, m.Mode)
	case pinned.RunID != m.RunID:
		return differ("run id", pinned.RunID, m.RunID)
	case pinned.Corpus != m.Corpus:
		return differ("corpus", pinned.Corpus, m.Corpus)
	case pinned.RecordsSHA256 != m.RecordsSHA256:
		return differ("records sha256", pinned.RecordsSHA256, m.RecordsSHA256)
	case !slices.Equal(pinned.Tasks, m.Tasks):
		return differ("tasks", fmt.Sprintf("%d task(s)", len(pinned.Tasks)), fmt.Sprintf("%d task(s), or others", len(m.Tasks)))
	case !slices.Equal(pinned.Arms, m.Arms):
		return differ("arms", pinned.Arms, m.Arms)
	case pinned.Replicates != m.Replicates:
		return differ("replicates", pinned.Replicates, m.Replicates)
	case pinned.ConfigDigest != m.ConfigDigest:
		return differ("config digest", pinned.ConfigDigest, m.ConfigDigest)
	default:
		return nil
	}
}

// cells returns task's cells, in the order they run: each replicate in
// turn, each arm in turn within it.
func (r *corpusRun) cells(task Task) []Cell {
	var out []Cell
	for rep := 1; rep <= r.manifest.Replicates; rep++ {
		for _, arm := range r.arms {
			out = append(out, Cell{Task: task, Arm: arm, Replicate: rep})
		}
	}
	return out
}

// cellDir is the cell's directory.
func (r *corpusRun) cellDir(c Cell) string {
	return filepath.Join(r.opts.Dir, "cells", c.Task.ID, c.System())
}

// work generates and scores every cell still to do, task by task, and
// returns why it stopped, or nil when nothing is left.
func (r *corpusRun) work(ctx context.Context) error {
	for _, task := range r.opts.Tasks {
		cells := r.cells(task)
		for _, cell := range cells {
			if ctx.Err() != nil {
				return fmt.Errorf("%w (before %s)", ErrCorpusInterrupted, cell.Key())
			}
			if exists(resultPath(r.cellDir(cell))) {
				continue
			}
			if err := r.ledger.admit(); err != nil {
				r.logger.LogAttrs(ctx, slog.LevelWarn, "fixrate: stopping at the spend cap",
					slog.String("next_cell", cell.Key()), slog.Float64("spent_usd", r.ledger.total()))
				return errors.Join(err, r.scoreAll(ctx))
			}
			r.logger.LogAttrs(ctx, slog.LevelInfo, "fixrate: cell started", slog.String("cell", cell.Key()))
			res, err := r.generate(ctx, cell)
			if err != nil {
				return err
			}
			r.ledger.record(res)
			r.logger.LogAttrs(ctx, slog.LevelInfo, "fixrate: cell finished", slog.String("cell", cell.Key()),
				slog.Float64("usd", res.UsageUSD), slog.Float64("corpus_usd", r.ledger.total()))
		}
		for _, cell := range cells {
			if err := r.score(ctx, cell); err != nil {
				return err
			}
		}
	}
	return nil
}

// scoreAll scores every cell that has a result and no verdict, which is what
// a corpus that is stopping for its cap still owes: scoring costs nothing.
func (r *corpusRun) scoreAll(ctx context.Context) error {
	for _, task := range r.opts.Tasks {
		for _, cell := range r.cells(task) {
			if err := r.score(ctx, cell); err != nil {
				return err
			}
		}
	}
	return nil
}

// score asks the evaluator for the verdict on a finished cell's patch, once:
// a cell that has a verdict, or no result yet, is left alone.
func (r *corpusRun) score(ctx context.Context, cell Cell) error {
	dir := r.cellDir(cell)
	if !exists(resultPath(dir)) || exists(verdictPath(dir)) {
		return nil
	}
	var res CellResult
	if err := readJSON(resultPath(dir), &res); err != nil {
		return err
	}
	v, err := r.opts.Evaluator.Evaluate(ctx, cell.Task, cell.System(), res.Patch)
	if ctx.Err() != nil {
		return fmt.Errorf("%w (while scoring %s)", ErrCorpusInterrupted, cell.Key())
	}
	if err != nil {
		return fmt.Errorf("fixrate: scoring %s: %w", cell.Key(), err)
	}
	return writeJSONAtomic(verdictPath(dir), CellVerdict{
		SchemaVersion: corpusSchemaVersion,
		System:        cell.System(),
		Verdict:       v,
		ScoredAt:      r.now().UTC(),
	})
}

// corpusLedger is the corpus's account of what it has spent, rebuilt from
// the cells on every run; see this file's "The spend cap".
type corpusLedger struct {
	cap         float64
	reservation float64
	recorded    float64
	charged     float64
	estimated   bool
}

// capSlack absorbs floating-point error in a comparison of dollar sums, so
// a cell whose reservation fits exactly is not refused for the rounding of
// what came before it.
const capSlack = 1e-9

// total is everything the ledger counts toward the cap.
func (l *corpusLedger) total() float64 { return l.recorded + l.charged }

// admit refuses the next cell when it could take the corpus past its cap.
func (l *corpusLedger) admit() error {
	if l.cap > 0 && l.total()+l.reservation > l.cap+capSlack {
		return fmt.Errorf("%w: $%.2f spent or charged, and the next cell may spend up to $%.2f (budget.max_usd), "+
			"which would pass the cap of $%.2f; raise it and run again to continue", ErrSpendCapReached,
			l.total(), l.reservation, l.cap)
	}
	return nil
}

// record adds a finished cell's spend.
func (l *corpusLedger) record(res CellResult) {
	l.recorded += res.UsageUSD
	l.estimated = l.estimated || res.UsageEstimated
}

// charge adds a charge for an attempt whose spend is unknowable.
func (l *corpusLedger) charge(usd float64) {
	if usd > 0 {
		l.charged += usd
		l.estimated = true
	}
}

// loadLedger rebuilds the ledger from every cell on disk.
func (r *corpusRun) loadLedger() error {
	for _, task := range r.opts.Tasks {
		for _, cell := range r.cells(task) {
			dir := r.cellDir(cell)
			att, err := readAttempts(dir)
			if err != nil {
				return err
			}
			r.ledger.charge(att.ChargedUSD)
			if !exists(resultPath(dir)) {
				if att.Open && att.Spending {
					// An attempt died while spending, and nothing
					// recorded what it spent. generate writes this
					// charge down when it next starts the cell; it is
					// counted now, so no cell before then is admitted
					// on a total that leaves it out.
					r.ledger.charge(r.ledger.reservation)
				}
				continue
			}
			var res CellResult
			if err := readJSON(resultPath(dir), &res); err != nil {
				return err
			}
			r.ledger.record(res)
		}
	}
	return nil
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
