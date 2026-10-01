//go:build fixrate

package fixrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file is the paired report: the only output of this harness that is
// a claim about belay rather than a measurement of it.
//
// # Why paired, and why that word is load-bearing
//
// Both arms run the SAME tasks (see control.go). That makes every task a
// matched pair, and the pairing is the most informative thing in the data:
// a task belay resolved and the control arm did not is direct evidence
// about orchestration, and a task both arms resolved is evidence about the
// backend, not about belay. An unpaired comparison -- two resolve rates and
// a difference-of-proportions interval -- throws exactly that away, and
// this file deliberately offers no such interval, so a reader cannot pick
// one up by accident. PairedComparison reports the four cells of the 2x2
// table and an exact McNemar test over the two discordant ones.
//
// # Cost per resolved task is the headline, not a footnote
//
// A resolve-rate delta on its own is not a result, because spending more is
// always available as a way to resolve more: belay invokes an agent up to
// five times per task where the control arm invokes it once, so belay is
// EXPECTED to cost more in total. The comparison only becomes a claim when
// the money is in it. So ArmSummary carries USDPerResolvedTask beside the
// rate, HumanSummary prints it on its own labelled line for both arms, and
// the ten-task fixture the tests use is arranged as the case that matters:
// belay costing more in total and less per resolved task, which a
// total-spend-only report reads backwards.
//
// # What this report cannot claim, however good the numbers look
//
// The statistics do not launder provenance, and nothing here pretends
// otherwise:
//
//   - In ModeReplay both arms are driven by a scripted, zero-cost
//     stand-in. A delta measured that way is a delta between two code
//     paths of belay's own, not between belay and a real agent. Both the
//     mode provenance and each arm provenance are copied into the report
//     so this cannot be quoted out of context.
//   - Resolving a task is not the same as writing the change a maintainer
//     would accept. Resolution is whatever the corpus's own check says it
//     is -- catalogued tests going green for fixtures/seeded-bug, the
//     dataset's own test sets for SWE-bench (see swebench.go) -- and no
//     check of that shape can see a fix that is ugly, unsafe or narrower
//     than the issue.
//   - A p-value is about one corpus. It says how surprising this pattern
//     of disagreements would be if the two arms were equally good; it says
//     nothing about a different corpus, a different backend, or a
//     different model.
//   - No discordant pairs means no evidence of a difference. It is not
//     evidence that the arms are equivalent, and Warnings says so in words
//     when it happens.

// PairedSchemaVersion is PairedResult's JSON schema version, bumped
// whenever a field is added, removed or reinterpreted incompatibly. It is
// deliberately separate from SchemaVersion: a Result and a PairedResult are
// different documents with different readers.
const PairedSchemaVersion = 1

// ErrUnpairedTask reports a PairedTask whose two halves do not describe the
// same task in the two expected arms.
//
// It is a refusal rather than a repair because the whole analysis rests on
// the pairing: a report that quietly compared task t01's belay outcome
// against task t99's control outcome would produce a McNemar p-value with
// no meaning at all, and nothing downstream could tell.
var ErrUnpairedTask = errors.New("fixrate: paired task's arms do not name the same task and the two expected arms")

// TaskOutcome is one task's result in one arm: whether it resolved, why,
// what it cost, and how long it took.
type TaskOutcome struct {
	// TaskID identifies the task within its corpus.
	TaskID string `json:"task_id"`
	// Arm records which arm produced this outcome, so an outcome lifted
	// out of the report still says what it measured.
	Arm Arm `json:"arm"`
	// Resolved is the corpus's own resolution verdict. Only a true here
	// counts toward a resolve rate.
	Resolved bool `json:"resolved"`
	// Reason says why the task landed where it did, in words.
	Reason string `json:"reason"`
	// UsageUSD is what this arm spent on this task, counted whether or
	// not the task resolved and whether or not the arm's call failed.
	UsageUSD float64 `json:"usage_usd"`
	// UsageEstimated mirrors belay.Usage.Estimated: whether UsageUSD came
	// from a reported figure or a fallback price table.
	UsageEstimated bool `json:"usage_estimated"`
	// Turns is the agent turns the arm reported taking, or 0 when the
	// backend does not report them.
	Turns int `json:"turns"`
	// WallClockSeconds is how long the arm took on this task, copied from
	// Result.WallClockSeconds: the arm's own work, measured with
	// RunOptions.Now, so a pinned clock keeps it -- and every report built
	// on it -- byte-reproducible.
	WallClockSeconds float64 `json:"wall_clock_seconds"`
	// Note is the arm's own account of how it ended -- the dispatcher's
	// Outcome.Note for the belay arm, the backend's error for the control
	// arm -- or empty.
	Note string `json:"note,omitempty"`

	// Allowance is the ceiling this outcome's arm was given, or nil when
	// the arm has no derived ceiling to audit.
	//
	// Only the control arm sets one. Its allowance is DERIVED from the
	// configuration (controlAllowance) rather than configured directly,
	// which makes it the one number a sceptical reader of a losing control
	// arm needs and cannot recompute: they do not have the configuration.
	// belay's own ceilings are its configuration, so there is nothing to
	// promote and resultTaskOutcome leaves this nil for the belay arm.
	Allowance *Allowance `json:"allowance,omitempty"`

	// Model is the model this outcome ran on, or empty when there is no
	// single one to name.
	//
	// Only the control arm records one, because its whole run is one call
	// on one model (ControlRunInfo.Model). The belay arm routes plan, code
	// and fix separately (config.Agent.ModelFor), so a belay task has no
	// single model and resultTaskOutcome deliberately leaves this empty
	// rather than picking the code node's and implying the others.
	Model string `json:"model,omitempty"`
}

// PairedTask is one task's outcome in both arms. Both halves must name the
// same TaskID and carry their own arm's label; see ErrUnpairedTask.
type PairedTask struct {
	TaskID  string      `json:"task_id"`
	Belay   TaskOutcome `json:"belay"`
	Control TaskOutcome `json:"control"`
}

// ArmSummary is one arm's rollup over every task.
//
// Provenance is placed immediately after Arm, before any number, in both
// the struct and the JSON, for the same reason Result puts it first:
// reading only the opening lines of an arm's summary must still tell you
// what its numbers measured.
type ArmSummary struct {
	Arm        Arm    `json:"arm"`
	Provenance string `json:"provenance"`

	// Model is the model every task in this arm was pinned to, or
	// "(mixed)" if the tasks disagree -- which is a wiring bug worth
	// seeing in the report rather than a fact to average over.
	Model string `json:"model,omitempty"`

	Tasks       int     `json:"tasks"`
	Resolved    int     `json:"resolved"`
	ResolveRate float64 `json:"resolve_rate"`
	// ResolveRateCI is the 95% Wilson score interval on ResolveRate. See
	// WilsonInterval for why Wilson and not the textbook approximation.
	ResolveRateCI Interval `json:"resolve_rate_ci_95"`

	// USDPerResolvedTask is the headline figure: total spend divided by
	// tasks resolved. It is nil, and omitted from the JSON as null, when
	// the arm resolved nothing -- undefined rather than infinite. See
	// USDPerResolved.
	USDPerResolvedTask *float64 `json:"usd_per_resolved_task"`

	UsageUSD float64 `json:"usage_usd"`
	// UsageEstimated is true when ANY task's spend was an estimate rather
	// than a figure the backend reported. It is deliberately the
	// pessimistic rollup: one estimated task makes the arm's total an
	// estimate, because a reader enforcing a budget against it must treat
	// it as lower-confidence.
	UsageEstimated bool `json:"usage_estimated"`

	WallClockSeconds float64 `json:"wall_clock_seconds"`

	// Allowance is the ceiling each of this arm's calls was given. It is
	// set for the control arm, whose allowance is derived and therefore
	// worth auditing, and nil for the belay arm, whose ceilings are its
	// own configuration.
	Allowance *Allowance `json:"allowance,omitempty"`
}

// PairedComparison is the paired analysis: the 2x2 table's four cells and
// an exact test over the two that carry information.
type PairedComparison struct {
	// BothResolved and NeitherResolved are the concordant cells. They
	// size the corpus and say how much of it either arm could reach, but
	// they carry no information about which arm is better and the McNemar
	// test correctly ignores them.
	BothResolved    int `json:"both_resolved"`
	NeitherResolved int `json:"neither_resolved"`

	// BelayOnly and ControlOnly are the discordant cells: tasks exactly
	// one arm resolved. These are the whole of the evidence about
	// orchestration.
	BelayOnly   int `json:"belay_only"`
	ControlOnly int `json:"control_only"`

	// ResolveRateDelta is belay's resolve rate minus the control arm's.
	// Positive means belay resolved more of the corpus.
	ResolveRateDelta float64 `json:"resolve_rate_delta"`

	// McNemarExactP is the two-sided exact McNemar p-value over
	// BelayOnly and ControlOnly. It is 1 when the arms disagreed about
	// nothing, which is an absence of evidence and not evidence of
	// equivalence. See ExactMcNemarP.
	McNemarExactP float64 `json:"mcnemar_exact_p"`
}

// PairedResult is one paired comparison's complete, machine-readable
// output.
type PairedResult struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          Mode   `json:"mode"`
	Provenance    string `json:"provenance"`

	GeneratedAt time.Time `json:"generated_at"`

	Belay      ArmSummary       `json:"belay"`
	Control    ArmSummary       `json:"control"`
	Comparison PairedComparison `json:"comparison"`

	Tasks []PairedTask `json:"tasks"`

	// Warnings lists conditions a reader must know about before quoting
	// any number above -- an empty corpus, no discordant pairs, an arm
	// whose spend was estimated.
	Warnings []string `json:"warnings,omitempty"`
}

// summarizePaired rolls pairs up into a PairedResult at the given mode and
// timestamp.
//
// It is a pure function of its arguments: no clock, no filesystem, no
// dispatcher, no agent. That is what lets the report's arithmetic be
// checked against hand-computed values (paired_test.go) instead of against
// whatever a multi-minute run happened to produce.
//
// It refuses rather than repairs two things. An unrecognized mode, because
// a report whose provenance line cannot be vouched for should not exist.
// And a pair whose halves disagree about which task or which arm they
// describe, because the entire analysis rests on the pairing and nothing
// downstream could detect a mismatch; see ErrUnpairedTask.
//
// It does NOT check RunOptions.Confirm: this function spends nothing. The
// live-spend tripwire belongs at the boundary where money is spent, which
// is Run (see ConfirmLiveSpend), and duplicating it here would suggest
// that passing this function is what makes a live run permitted.
func summarizePaired(mode Mode, generatedAt time.Time, pairs []PairedTask) (*PairedResult, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownMode, mode)
	}
	for i, p := range pairs {
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("fixrate: paired task %d (%q): %w", i, p.TaskID, err)
		}
	}

	belayOutcomes := make([]TaskOutcome, 0, len(pairs))
	controlOutcomes := make([]TaskOutcome, 0, len(pairs))
	var cmp PairedComparison
	for _, p := range pairs {
		belayOutcomes = append(belayOutcomes, p.Belay)
		controlOutcomes = append(controlOutcomes, p.Control)
		switch {
		case p.Belay.Resolved && p.Control.Resolved:
			cmp.BothResolved++
		case p.Belay.Resolved:
			cmp.BelayOnly++
		case p.Control.Resolved:
			cmp.ControlOnly++
		default:
			cmp.NeitherResolved++
		}
	}

	belaySummary := summarizeArm(ArmBelay, belayOutcomes)
	controlSummary := summarizeArm(ArmControl, controlOutcomes)
	cmp.ResolveRateDelta = belaySummary.ResolveRate - controlSummary.ResolveRate
	cmp.McNemarExactP = ExactMcNemarP(cmp.BelayOnly, cmp.ControlOnly)

	return &PairedResult{
		SchemaVersion: PairedSchemaVersion,
		Mode:          mode,
		Provenance:    mode.provenance(),
		GeneratedAt:   generatedAt,
		Belay:         belaySummary,
		Control:       controlSummary,
		Comparison:    cmp,
		Tasks:         pairs,
		Warnings:      pairedWarnings(len(pairs), cmp, belaySummary, controlSummary),
	}, nil
}

// validate reports whether p's halves describe the same task in the two
// expected arms.
func (p PairedTask) validate() error {
	switch {
	case p.Belay.TaskID != p.TaskID:
		return fmt.Errorf("%w: belay outcome names task %q", ErrUnpairedTask, p.Belay.TaskID)
	case p.Control.TaskID != p.TaskID:
		return fmt.Errorf("%w: control outcome names task %q", ErrUnpairedTask, p.Control.TaskID)
	case p.Belay.Arm != ArmBelay:
		return fmt.Errorf("%w: the belay half is labelled %q", ErrUnpairedTask, p.Belay.Arm)
	case p.Control.Arm != ArmControl:
		return fmt.Errorf("%w: the control half is labelled %q", ErrUnpairedTask, p.Control.Arm)
	default:
		return nil
	}
}

// summarizeArm rolls one arm's outcomes up.
//
// The model and the allowance are PROMOTED from the outcomes rather than
// passed in, which is what keeps summarizePaired a pure function of the
// pairs it was handed. Both are promoted only when every task in the arm
// agrees: two tasks in one arm on different models, or under different
// allowances, is a wiring bug that would invalidate the comparison, and
// the report must show it rather than average it away.
func summarizeArm(arm Arm, outcomes []TaskOutcome) ArmSummary {
	s := ArmSummary{
		Arm:        arm,
		Provenance: arm.provenance(),
		Tasks:      len(outcomes),
	}
	models := make(map[string]bool)
	allowances := make(map[Allowance]bool)
	for _, o := range outcomes {
		if o.Allowance != nil {
			allowances[*o.Allowance] = true
		}
		if o.Resolved {
			s.Resolved++
		}
		s.UsageUSD += o.UsageUSD
		s.UsageEstimated = s.UsageEstimated || o.UsageEstimated
		s.WallClockSeconds += o.WallClockSeconds
		if o.Model != "" {
			models[o.Model] = true
		}
	}
	if s.Tasks > 0 {
		s.ResolveRate = float64(s.Resolved) / float64(s.Tasks)
	}
	s.ResolveRateCI = WilsonInterval(s.Resolved, s.Tasks, Z95)
	if perFix, ok := USDPerResolved(s.UsageUSD, s.Resolved); ok {
		s.USDPerResolvedTask = &perFix
	}
	switch len(models) {
	case 0:
		// nothing recorded
	case 1:
		for m := range models {
			s.Model = m
		}
	default:
		s.Model = "(mixed)"
	}
	if len(allowances) == 1 {
		for a := range allowances {
			s.Allowance = &a
		}
	}
	return s
}

// pairedWarnings lists what a reader must know before quoting any number
// in the report.
func pairedWarnings(tasks int, cmp PairedComparison, arms ...ArmSummary) []string {
	var w []string
	if tasks == 0 {
		w = append(w, "no tasks were compared: every rate below is undefined and every interval is the whole range [0,1]")
	}
	if tasks > 0 && cmp.BelayOnly == 0 && cmp.ControlOnly == 0 {
		w = append(w, "the two arms disagreed about nothing, so mcnemar_exact_p is 1 by construction; "+
			"that is an ABSENCE of evidence for a difference, not evidence that the arms are equivalent")
	}
	for _, a := range arms {
		if a.UsageEstimated {
			w = append(w, fmt.Sprintf("the %s arm's usage_usd includes at least one estimated figure "+
				"rather than a cost the backend reported; treat usd_per_resolved_task as lower-confidence", a.Arm))
		}
		if a.Tasks > 0 && a.Resolved == 0 {
			w = append(w, fmt.Sprintf("the %s arm resolved nothing, so it has no usd_per_resolved_task", a.Arm))
		}
	}
	return w
}

// MarshalIndentJSON renders r as indented JSON, matching Result.
// MarshalIndentJSON's style (2-space indent, HTML escaping off).
func (r *PairedResult) MarshalIndentJSON() ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// HumanSummary renders r as a short, human-readable report.
//
// The order is fixed and is itself the point: the mode's provenance, then
// each arm's provenance, then a table in which USD per resolved task sits
// in the same block as the resolve rate rather than below the fold, then
// the paired analysis, then warnings. A reader who stops after the first
// screen should have the labels and the cost, not just the rates.
func (r *PairedResult) HumanSummary() string {
	var b strings.Builder

	fmt.Fprintln(&b, r.Provenance)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, r.Belay.Provenance)
	fmt.Fprintln(&b, r.Control.Provenance)
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "%-9s %-19s %-24s %-13s %s\n", "arm", "resolved", "95% CI (Wilson)", "total spend", "USD PER RESOLVED TASK")
	writeArm(&b, r.Belay)
	writeArm(&b, r.Control)
	fmt.Fprintln(&b)

	c := r.Comparison
	fmt.Fprintf(&b, "resolve-rate delta (belay - control): %+.4f\n", c.ResolveRateDelta)
	fmt.Fprintf(&b, "paired outcomes: both=%d belay_only=%d control_only=%d neither=%d\n",
		c.BothResolved, c.BelayOnly, c.ControlOnly, c.NeitherResolved)
	fmt.Fprintf(&b, "McNemar (exact, two-sided) over the %d discordant pair(s): p = %.6g\n",
		c.BelayOnly+c.ControlOnly, c.McNemarExactP)
	fmt.Fprintf(&b, "wall clock: belay %.1fs, control %.1fs\n", r.Belay.WallClockSeconds, r.Control.WallClockSeconds)
	if a := r.Control.Allowance; a != nil {
		fmt.Fprintf(&b, "control-arm allowance per task: %d turn(s), %s, derived from the %d agent-invoking "+
			"node execution(s) belay's graph could have made\n", a.MaxTurns, a.Timeout, a.AgentCalls)
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintln(&b)
		for _, w := range r.Warnings {
			fmt.Fprintln(&b, "warning:", w)
		}
	}
	return b.String()
}

// writeArm prints one arm's row of HumanSummary's table.
func writeArm(b *strings.Builder, a ArmSummary) {
	perFix := "n/a (nothing resolved)"
	if a.USDPerResolvedTask != nil {
		perFix = fmt.Sprintf("$%.4f", *a.USDPerResolvedTask)
	}
	estimated := ""
	if a.UsageEstimated {
		estimated = " (estimated)"
	}
	fmt.Fprintf(b, "%-9s %-19s [%.4f, %.4f]%9s $%-12.4f %s%s\n",
		a.Arm,
		fmt.Sprintf("%d/%d (%.1f%%)", a.Resolved, a.Tasks, a.ResolveRate*100),
		a.ResolveRateCI.Lo, a.ResolveRateCI.Hi, "",
		a.UsageUSD,
		perFix, estimated)
}

// resultTaskOutcome converts either arm's Result into the TaskOutcome a
// paired report consumes.
//
// It is ONE function for both arms on purpose. A task's resolution must be
// decided by the identical rule in both arms or the comparison is between
// two different questions; with one conversion over one Result type --
// produced by one shared pipeline (runArm) -- that is structural, not a
// promise each caller has to keep.
//
// # The resolution rule
//
// A task is resolved only when EVERY defect it selected came back
// StatusRepaired. Anything else -- one still broken, one unverified, and
// above all one StatusInvalidRepair -- leaves the task unresolved. Crediting
// a task with an invalid repair in it would defeat the cheat detector at the
// rollup after evaluate.go went to such lengths to catch it one defect at a
// time.
//
// A Result that selected no defects at all is not a resolved task; it is an
// empty one, and resolving nothing is not a success.
//
// The arm label is copied from the Result rather than assumed, so a Result
// with no arm -- one built by hand rather than by Run or RunControl -- is
// refused by summarizePaired instead of being paired as whichever arm the
// caller happened to put it in.
func resultTaskOutcome(taskID string, r *Result) TaskOutcome {
	t := r.Totals
	resolved := t.Selected > 0 && t.Repaired == t.Selected

	var reason string
	switch {
	case t.Selected == 0:
		reason = "no defects were selected for this task, so nothing could be resolved"
	case resolved:
		reason = fmt.Sprintf("all %d selected defect(s) repaired", t.Selected)
	default:
		reason = fmt.Sprintf("%d of %d selected defect(s) repaired (not_repaired=%d invalid_repair=%d unverified=%d)",
			t.Repaired, t.Selected, t.NotRepaired, t.InvalidRepair, t.Unverified)
	}

	o := TaskOutcome{
		TaskID:           taskID,
		Arm:              r.Arm,
		Resolved:         resolved,
		Reason:           reason,
		UsageUSD:         r.UsageUSD,
		UsageEstimated:   r.UsageEstimated,
		WallClockSeconds: r.WallClockSeconds,
		Note:             r.BelayNote,
	}
	if c := r.Control; c != nil {
		allowance := c.Allowance
		o.Turns = c.Turns
		o.Model = c.Model
		o.Allowance = &allowance
		o.Note = c.RunErr
	}
	return o
}
