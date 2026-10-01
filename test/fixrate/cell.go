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
	"time"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/linter"
	"github.com/dhaam-ai/belay/internal/nodes"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// This file is one cell: the files that make it durable, one attempt at it
// from start to result, and the two arms' drive steps. corpus.go's header
// comment says what each file means to a resumed run; this file keeps those
// promises.
//
// # When the harness fails, not the arm
//
// A finished cell is never run again, so a cell must not be recorded as
// finished when what failed was the harness rather than the arm. Two cases
// are told apart, and each stops the corpus with ErrInfrastructure, records
// nothing for the cell, and leaves it resumable:
//
//   - The belay arm's test runner or lint gate could not run at all -- a
//     container that died, a daemon that went away, a linter that is not
//     installed. Those adapters are the workspace's, supplied by the
//     harness. belay's own rule is that a step that cannot reach a verdict
//     ends the run as failed; for the benchmark that would score a Docker
//     hiccup as belay losing the task. So the adapters are wrapped, and an
//     error from either cancels the run's context: the dispatcher pauses the
//     run, as it does for any cancellation, and a resume re-runs that one
//     step without planning or coding again.
//   - In a live corpus, an arm that failed having spent nothing never
//     reached the task. An expired key, or credit that ran out in the night,
//     fails every call before it costs anything; recorded as outcomes, the
//     rest of a thousand-cell run would become a thousand failures in
//     minutes, none of which could be run again. A belay run that failed
//     that way is discarded when the cell is next attempted -- it did
//     nothing and spent nothing -- and a control call that did is simply
//     made again.
//
// A call that spent something and then failed -- hit its turn limit, ran out
// of time, was refused mid-way -- did reach the task, and its failure is the
// arm's outcome, in both arms alike.

// CellResult is what one cell's work produced, written once, atomically, as
// result.json. It is private to the corpus directory: it holds the patch,
// which is derived from dataset content and is never published (see
// CorpusReport).
type CellResult struct {
	SchemaVersion int    `json:"schema_version"`
	TaskID        string `json:"task_id"`
	Arm           Arm    `json:"arm"`
	Replicate     int    `json:"replicate"`
	// Attempts is how many attempts at the cell were started, this one
	// included. More than one means it was interrupted at least once.
	Attempts int `json:"attempts"`
	// Patch is the cell's change, as the patch the evaluator scores.
	Patch string `json:"patch"`
	// UsageUSD is what the arm reported spending on the cell: the belay
	// run's cumulative ledger, or the control call's reported cost. It does
	// not include charges for attempts whose spend was never reported; see
	// cellAttempts.ChargedUSD.
	UsageUSD       float64 `json:"usage_usd"`
	UsageEstimated bool    `json:"usage_estimated"`
	// WallClockSeconds is the time the cell's attempts took, interrupted
	// ones included, as far as they could be measured.
	WallClockSeconds float64 `json:"wall_clock_seconds"`
	// Model, Turns and Allowance describe the control arm's call.
	Model     string     `json:"model,omitempty"`
	Turns     int        `json:"turns,omitempty"`
	Allowance *Allowance `json:"allowance,omitempty"`
	// BelayStatus and BelaySteps describe the belay arm's run.
	BelayStatus string `json:"belay_status,omitempty"`
	BelaySteps  int    `json:"belay_steps,omitempty"`
	// Note is the arm's own account of how it ended.
	Note string `json:"note,omitempty"`
	// Adapters says what the workspace supplied the belay arm's graph with.
	Adapters string `json:"adapters,omitempty"`
	// ShippedGate is linter.Explain's problem when belay as shipped would
	// have found no lint gate in the workspace and refused to start a run
	// there, or empty when it would have found one.
	ShippedGate string    `json:"shipped_gate,omitempty"`
	FinishedAt  time.Time `json:"finished_at"`
}

// CellVerdict is the evaluator's verdict on a cell's patch, written once,
// atomically, as verdict.json.
type CellVerdict struct {
	SchemaVersion int       `json:"schema_version"`
	System        string    `json:"system"`
	Verdict       Verdict   `json:"verdict"`
	ScoredAt      time.Time `json:"scored_at"`
}

// cellAttempts is attempts.json: the bookkeeping that lets a resumed corpus
// tell an attempt that is under way from one that died.
type cellAttempts struct {
	// Started counts attempts begun.
	Started int `json:"started"`
	// Open is true from the moment an attempt starts until it ends, either
	// way. Found true when an attempt starts, it means the last one died
	// without closing.
	Open bool `json:"open"`
	// Spending is true while the open attempt is doing work that costs
	// money and has not yet been recorded anywhere -- the belay run, or
	// the control call before its response is kept.
	Spending bool `json:"spending"`
	// ChargedUSD is what the corpus has charged this cell for attempts
	// whose spend was never reported: the cell's reservation for each.
	ChargedUSD float64 `json:"charged_usd"`
	// Seconds is the wall clock of attempts that ended without a result.
	Seconds float64 `json:"seconds"`
}

// controlResponse is response.json: the control call's outcome, kept the
// moment the call returns, so it is never made again.
type controlResponse struct {
	Usage     belay.Usage `json:"usage"`
	Turns     int         `json:"turns"`
	Text      string      `json:"text"`
	Model     string      `json:"model"`
	Allowance Allowance   `json:"allowance"`
	RunErr    string      `json:"run_err,omitempty"`
}

// armRecord is what an arm's drive step hands back to generate.
type armRecord struct {
	usage       belay.Usage
	model       string
	turns       int
	allowance   *Allowance
	belayStatus string
	belaySteps  int
	note        string
}

// errCellInterrupted marks an attempt cut off by the corpus's context.
var errCellInterrupted = errors.New("fixrate: the cell was interrupted")

func resultPath(dir string) string   { return filepath.Join(dir, "result.json") }
func verdictPath(dir string) string  { return filepath.Join(dir, "verdict.json") }
func attemptsPath(dir string) string { return filepath.Join(dir, "attempts.json") }
func responsePath(dir string) string { return filepath.Join(dir, "response.json") }

// generate makes one attempt at a cell, from provisioning its workspace to
// writing its result, and returns the result.
//
// An attempt that does not finish leaves no result: it closes attempts.json
// saying so, and returns an error wrapping ErrCorpusInterrupted when the
// corpus's context was cancelled, or the cause otherwise.
func (r *corpusRun) generate(ctx context.Context, cell Cell) (CellResult, error) {
	dir := r.cellDir(cell)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return CellResult{}, fmt.Errorf("fixrate: create %s: %w", dir, err)
	}
	att, err := readAttempts(dir)
	if err != nil {
		return CellResult{}, err
	}
	if att.Open && att.Spending {
		// The last attempt died while spending, and nothing recorded what
		// it spent. loadLedger has already counted this charge; it is
		// written down here so it outlives this run.
		att.ChargedUSD += r.ledger.reservation
	}
	att.Started++
	att.Open, att.Spending = true, false
	if err := writeJSONAtomic(attemptsPath(dir), att); err != nil {
		return CellResult{}, err
	}

	start := r.now()
	fresh := cell.Arm == ArmControl && !exists(responsePath(dir))
	if cell.Arm == ArmBelay && r.discardable(dir) {
		// A live belay run that failed having spent nothing reached
		// nothing; see this file's header comment.
		if err := os.RemoveAll(filepath.Join(dir, ".belay")); err != nil {
			return CellResult{}, r.abandon(ctx, cell, dir, &att, start, fmt.Errorf("fixrate: discard %s's empty run: %w", cell.Key(), err))
		}
		fresh = true
	}
	ws, err := r.opts.Provisioner.Provision(ctx, cell, dir, fresh)
	if err != nil {
		return CellResult{}, r.abandon(ctx, cell, dir, &att, start, fmt.Errorf("fixrate: provisioning %s: %w", cell.Key(), err))
	}

	var rec armRecord
	switch cell.Arm {
	case ArmBelay:
		rec, err = r.driveBelay(ctx, cell, dir, ws, &att)
	case ArmControl:
		rec, err = r.driveControl(ctx, cell, dir, ws, &att)
	default:
		err = fmt.Errorf("%w: unknown arm %q", errCorpusConfig, cell.Arm)
	}
	if err != nil {
		return CellResult{}, r.abandon(ctx, cell, dir, &att, start, err)
	}
	if r.opts.Mode == ModeReplay && (rec.usage.USD != 0 || rec.usage.InputTokens != 0 || rec.usage.OutputTokens != 0) {
		return CellResult{}, r.abandon(ctx, cell, dir, &att, start, fmt.Errorf("%w (cell %s recorded $%.6f)",
			ErrReplayModeSpentMoney, cell.Key(), rec.usage.USD))
	}

	patch, err := cellPatch(ctx, ws.Dir, ws.Base, filepath.Join(dir, "patch.index"))
	if err != nil {
		return CellResult{}, r.abandon(ctx, cell, dir, &att, start, err)
	}
	result := CellResult{
		SchemaVersion:    corpusSchemaVersion,
		TaskID:           cell.Task.ID,
		Arm:              cell.Arm,
		Replicate:        cell.Replicate,
		Attempts:         att.Started,
		Patch:            patch,
		UsageUSD:         rec.usage.USD,
		UsageEstimated:   rec.usage.Estimated,
		WallClockSeconds: att.Seconds + r.now().Sub(start).Seconds(),
		Model:            rec.model,
		Turns:            rec.turns,
		Allowance:        rec.allowance,
		BelayStatus:      rec.belayStatus,
		BelaySteps:       rec.belaySteps,
		Note:             rec.note,
		FinishedAt:       r.now().UTC(),
	}
	if cell.Arm == ArmBelay {
		result.Adapters = ws.Adapters
	}
	if why, missing := linter.Explain(ws.Dir); missing {
		result.ShippedGate = why.Problem
	}
	if err := writeJSONAtomic(resultPath(dir), result); err != nil {
		return CellResult{}, r.abandon(ctx, cell, dir, &att, start, err)
	}

	// From here the cell is finished, whatever happens next: a crash now
	// leaves attempts.json open, but a resumed run skips any cell with a
	// result.
	att.Open = false
	if err := writeJSONAtomic(attemptsPath(dir), att); err != nil {
		return result, err
	}
	if err := r.opts.Provisioner.Release(ctx, cell, dir); err != nil {
		r.logger.LogAttrs(ctx, slog.LevelWarn, "fixrate: could not release a finished cell's workspace",
			slog.String("cell", cell.Key()), slog.String("error", err.Error()))
	}
	return result, nil
}

// abandon closes an attempt that ended without a result. When the corpus's
// context was cancelled it returns an error wrapping ErrCorpusInterrupted;
// otherwise cause. An attempt cut off while spending is charged its
// reservation, because nothing reported what it spent.
func (r *corpusRun) abandon(ctx context.Context, cell Cell, dir string, att *cellAttempts, start time.Time, cause error) error {
	if att.Spending {
		att.ChargedUSD += r.ledger.reservation
		r.ledger.charge(r.ledger.reservation)
	}
	att.Open, att.Spending = false, false
	att.Seconds += r.now().Sub(start).Seconds()
	writeErr := writeJSONAtomic(attemptsPath(dir), *att)
	if ctx.Err() != nil || errors.Is(cause, errCellInterrupted) {
		return errors.Join(fmt.Errorf("%w (in %s)", ErrCorpusInterrupted, cell.Key()), writeErr)
	}
	return errors.Join(cause, writeErr)
}

// spending records, durably, that the open attempt is about to spend money
// nothing will record until it is done -- or that it has stopped.
func (r *corpusRun) spending(dir string, att *cellAttempts, on bool) error {
	att.Spending = on
	return writeJSONAtomic(attemptsPath(dir), *att)
}

// driveBelay is the belay arm: belay's real, unmodified dispatcher over
// nodes.Default's graph, with the workspace's own test runner and lint gate,
// started on a first attempt and resumed from belay's own journal on every
// later one. Dispatcher.Resume does both: it starts a run with no history,
// continues one a crash or an interruption cut off, re-running only the
// step that was cut off, and returns the outcome of one that already ended.
//
// The run directory sits beside the workspace, not in it, so the journal,
// the plan and every artifact stay out of the cell's patch: the layout's
// work tree is the workspace (state.Layout.WithWorkTree), its run directory
// the cell's own .belay/runs/belay.
func (r *corpusRun) driveBelay(ctx context.Context, cell Cell, dir string, ws Workspace, att *cellAttempts) (armRecord, error) {
	layout, err := state.NewLayout(dir, belayCellRunID)
	if err == nil {
		layout, err = layout.WithWorkTree(ws.Dir)
	}
	if err != nil {
		return armRecord{}, fmt.Errorf("fixrate: belay run layout for %s: %w", cell.Key(), err)
	}
	store := state.NewStore(layout)
	if !exists(layout.ManifestPath()) {
		// state.json first and the manifest last, as every run creator
		// does: a manifest is what makes a run findable.
		if err := os.MkdirAll(layout.RunDir(), 0o750); err != nil {
			return armRecord{}, fmt.Errorf("fixrate: create %s: %w", layout.RunDir(), err)
		}
		if err := store.SaveState(state.NewState(cell.Task.Goal)); err != nil {
			return armRecord{}, fmt.Errorf("fixrate: save the belay run's state: %w", err)
		}
		if err := store.SaveManifest(state.NewManifest(r.now(), belayCellRunID, ws.Dir, cell.Task.Goal, r.cfg)); err != nil {
			return armRecord{}, fmt.Errorf("fixrate: save the belay run's manifest: %w", err)
		}
	}
	registry, err := nodes.Default(r.cfg)
	if err != nil {
		return armRecord{}, fmt.Errorf("fixrate: wire the belay graph: %w", err)
	}
	// The run gets its own context so a harness adapter that cannot run
	// can stop it, as a pause; see this file's header comment.
	runCtx, stopRun := context.WithCancelCause(ctx)
	defer stopRun(nil)
	dispatcher, err := graph.NewDispatcher(graph.Options{
		Store:         store,
		Registry:      registry,
		Config:        r.cfg,
		Logger:        r.logger,
		Now:           r.now,
		Agent:         r.opts.AgentFor(cell),
		Runner:        guardedRunner{TestRunner: ws.Runner, stop: stopRun},
		Linter:        guardedLinter{Linter: ws.Linter, stop: stopRun},
		PauseOnCancel: true,
	})
	if err != nil {
		return armRecord{}, fmt.Errorf("fixrate: construct the dispatcher: %w", err)
	}

	if err := r.spending(dir, att, true); err != nil {
		return armRecord{}, err
	}
	outcome, runErr := dispatcher.Resume(runCtx)
	if cause := context.Cause(runCtx); ctx.Err() == nil && errors.Is(cause, ErrInfrastructure) {
		// The run paused at a step boundary with everything it spent in
		// its journal: nothing is lost, so nothing is charged.
		return armRecord{}, errors.Join(cause, r.spending(dir, att, false))
	}
	if errors.Is(runErr, graph.ErrInterrupted) || ctx.Err() != nil {
		return armRecord{}, fmt.Errorf("%w: %w", errCellInterrupted, runErr)
	}
	if err := r.spending(dir, att, false); err != nil {
		return armRecord{}, err
	}

	// Any other error is the run's outcome -- a failed step, a limit it
	// stopped at, a run that had already ended -- not a reason to give up
	// on the cell. The patch is whatever the run left in the workspace.
	man, err := store.LoadManifest()
	if err != nil {
		return armRecord{}, fmt.Errorf("fixrate: read the belay run's manifest: %w", err)
	}
	note := outcome.Note
	if note == "" && runErr != nil {
		note = runErr.Error()
	}
	if r.opts.Mode == ModeLive && emptyFailure(man.Status) && zeroUsage(outcome.Usage) {
		return armRecord{}, fmt.Errorf("%w: %s's belay run failed having spent nothing, so it never reached the task: %s",
			ErrInfrastructure, cell.Key(), note)
	}
	return armRecord{
		usage:       outcome.Usage,
		belayStatus: man.Status.String(),
		belaySteps:  man.Step,
		note:        note,
	}, nil
}

// driveControl is the control arm: runControl's one call, made once. A
// response kept by an earlier attempt is used as it is, and a call cut off
// by the corpus's context is reported as an interruption, never as the
// control arm's answer.
func (r *corpusRun) driveControl(ctx context.Context, cell Cell, dir string, ws Workspace, att *cellAttempts) (armRecord, error) {
	var resp controlResponse
	switch err := readJSON(responsePath(dir), &resp); {
	case err == nil:
		return controlRecord(resp), nil
	case !errors.Is(err, os.ErrNotExist):
		return armRecord{}, err
	}

	if err := r.spending(dir, att, true); err != nil {
		return armRecord{}, err
	}
	res, err := runControl(ctx, ws.Dir, cell.Task.Goal, r.opts.AgentFor(cell), r.cfg)
	if err != nil {
		return armRecord{}, err
	}
	if res.RunErr != "" && ctx.Err() != nil {
		return armRecord{}, fmt.Errorf("%w: %s", errCellInterrupted, res.RunErr)
	}
	if r.opts.Mode == ModeLive && res.RunErr != "" && zeroUsage(res.Usage) {
		// The backend reported spending nothing, so nothing is lost and
		// nothing is charged; see this file's header comment.
		return armRecord{}, errors.Join(fmt.Errorf("%w: %s's control call failed having spent nothing, so it never reached the task: %s",
			ErrInfrastructure, cell.Key(), res.RunErr), r.spending(dir, att, false))
	}
	// A conversion, not a copy field by field: a field runControl gains
	// must be kept too, or a resumed cell would lose it, and the
	// conversion stops compiling until it is.
	resp = controlResponse(res)
	if err := writeJSONAtomic(responsePath(dir), resp); err != nil {
		return armRecord{}, err
	}
	if err := r.spending(dir, att, false); err != nil {
		return armRecord{}, err
	}
	return controlRecord(resp), nil
}

// discardable reports whether the cell under dir holds a belay run a live
// corpus should start again rather than resume: one that failed having spent
// nothing. See this file's header comment.
func (r *corpusRun) discardable(dir string) bool {
	if r.opts.Mode != ModeLive {
		return false
	}
	layout, err := state.NewLayout(dir, belayCellRunID)
	if err != nil {
		return false
	}
	man, err := state.LoadManifest(layout.ManifestPath())
	if err != nil {
		return false
	}
	return emptyFailure(man.Status) && man.Budget.SpentUSD == 0
}

// emptyFailure reports whether a run that ended with status could be one that
// failed before doing anything: failed or aborted, rather than completed or
// paused at one of belay's own limits.
func emptyFailure(status state.RunStatus) bool {
	return status == state.RunStatusFailed || status == state.RunStatusAborted
}

// zeroUsage reports whether u records no spend at all.
func zeroUsage(u belay.Usage) bool { return u.USD == 0 && u.InputTokens == 0 && u.OutputTokens == 0 }

// guardedRunner is the belay arm's test runner, wrapped so that a test run
// that could not happen stops the run as a pause rather than ending it; see
// this file's header comment.
type guardedRunner struct {
	belay.TestRunner
	stop context.CancelCauseFunc
}

// Test implements belay.TestRunner.
func (g guardedRunner) Test(ctx context.Context, dir string) (belay.TestReport, error) {
	report, err := g.TestRunner.Test(ctx, dir)
	if err != nil && ctx.Err() == nil {
		g.stop(fmt.Errorf("%w: the belay arm's test runner %q could not run: %w", ErrInfrastructure, g.Name(), err))
	}
	return report, err
}

// guardedLinter is guardedRunner's counterpart for the lint gate.
type guardedLinter struct {
	belay.Linter
	stop context.CancelCauseFunc
}

// Lint implements belay.Linter.
func (g guardedLinter) Lint(ctx context.Context, dir string) (belay.QualityReport, error) {
	report, err := g.Linter.Lint(ctx, dir)
	if err != nil && ctx.Err() == nil {
		g.stop(fmt.Errorf("%w: the belay arm's lint gate %q could not run: %w", ErrInfrastructure, g.Name(), err))
	}
	return report, err
}

// controlRecord turns a kept control response into an armRecord.
func controlRecord(resp controlResponse) armRecord {
	allowance := resp.Allowance
	return armRecord{
		usage: resp.Usage, model: resp.Model, turns: resp.Turns, allowance: &allowance, note: resp.RunErr,
	}
}

// readAttempts reads a cell's attempts.json, or returns the zero value for a
// cell never attempted.
func readAttempts(dir string) (cellAttempts, error) {
	var att cellAttempts
	if err := readJSON(attemptsPath(dir), &att); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cellAttempts{}, err
	}
	return att, nil
}

// readJSON decodes the JSON file at path into v. A missing file is returned
// as an error satisfying errors.Is(err, os.ErrNotExist).
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path) //nolint:gosec // a file of the corpus's own
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("fixrate: %s: %w", path, err)
	}
	return nil
}

// writeJSONAtomic writes v as indented JSON to path, atomically.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("fixrate: encode %s: %w", path, err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}
