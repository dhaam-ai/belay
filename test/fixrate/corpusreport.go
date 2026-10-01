//go:build fixrate

package fixrate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// This file is the corpus's report, the one output of a corpus run that may
// leave the machine it ran on.
//
// # Aggregates only, and why that is enforced rather than promised
//
// The SWE-bench dataset declares no licence (swebench.go, "Licence"), so
// until one is established nothing derived from its instances is published:
// not a patch, which is an edit to a repository the instance names, not a
// trajectory -- what an agent said and did -- and not a problem statement
// or test name. A cell's own files hold all of those, because a resumed
// run needs them; they stay in the corpus directory. The report carries
// numbers about the corpus as a whole: rates and intervals, costs, the
// paired 2x2 table, A/A agreement, and counts of what the evaluation could
// and could not do.
//
// It carries no per-task rows at all, not even task ids with a resolved
// flag. PairedResult has them, for a corpus whose tasks are this
// repository's own (fixtures/seeded-bug); the corpus report builds the same
// numbers through summarizePaired and copies out only the aggregates.
// TestCorpusReport_PublishesAggregatesOnly plants marker text in every
// per-instance field a cell can hold and fails if any of it, or any task id,
// appears in the report's JSON or its summary.
//
// # What it can claim
//
// What README.md's SWE-bench section says, and nothing more: in live mode, a
// resolve rate is the share of the selected tasks whose dataset-supplied
// tests went from failing to passing without breaking the ones that passed
// before, as SWE-bench's own harness judged it; the delta between the arms is
// belay's orchestration, for that backend, that model and that
// configuration. In scripted mode it is a measurement of this harness's
// plumbing. The report's provenance lines say which.

// CorpusReportSchemaVersion versions CorpusReport's JSON.
const CorpusReportSchemaVersion = 1

// sweBenchScoring is the scoring disclosure every corpus report carries.
const sweBenchScoring = "scored by SWE-bench's official harness (swebench " + SWEBenchHarnessVersion +
	", python -m swebench.harness.run_evaluation): its per-instance verdict is the score. This package's port of " +
	"its grading rule re-grades the same test statuses as a cross-check; a disagreement is counted below and " +
	"never changes the score."

// CorpusReport is a corpus's aggregate report; see this file's header
// comment for what it holds and why that is all it holds.
type CorpusReport struct {
	SchemaVersion int    `json:"schema_version"`
	Mode          Mode   `json:"mode"`
	Provenance    string `json:"provenance"`
	Corpus        string `json:"corpus"`
	Scoring       string `json:"scoring"`
	Selection     string `json:"selection"`
	// Adapters says what the belay arm's test node and quality gate were,
	// where they were supplied by the harness rather than shipped.
	Adapters    []string  `json:"adapters,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	// Status is CorpusComplete, or why the corpus stopped.
	Status string `json:"status"`

	// Tasks is the size of the corpus; Paired is how many of its tasks
	// both arms' first replicates have been scored on, which is the n of
	// the comparison below.
	Tasks  int `json:"tasks"`
	Paired int `json:"paired"`

	Belay      ArmSummary       `json:"belay"`
	Control    ArmSummary       `json:"control"`
	Comparison PairedComparison `json:"comparison"`

	// Replicates is the A/A comparison: each arm against itself, run
	// twice, which is the run-to-run noise the arm delta must be read
	// against. Present only for a corpus with a second replicate.
	Replicates []ReplicateAgreement `json:"replicates,omitempty"`

	// Evaluation counts, per arm and replicate, what the evaluation could
	// and could not do.
	Evaluation []EvaluationCounts `json:"evaluation"`

	// ShippedGateRefused counts the tasks on which belay as shipped would
	// have found no lint gate and refused to start a run, which the
	// harness's own gate (Adapters) stood in for.
	ShippedGateRefused int `json:"shipped_gate_refused"`

	Spend    Spend    `json:"spend"`
	Warnings []string `json:"warnings,omitempty"`
}

// ReplicateAgreement is one arm's A/A comparison: its first and second
// replicates over the tasks both were scored on.
type ReplicateAgreement struct {
	Arm             Arm     `json:"arm"`
	Tasks           int     `json:"tasks"`
	BothResolved    int     `json:"both_resolved"`
	NeitherResolved int     `json:"neither_resolved"`
	FirstOnly       int     `json:"first_only"`
	SecondOnly      int     `json:"second_only"`
	Agreement       float64 `json:"agreement"`
	McNemarExactP   float64 `json:"mcnemar_exact_p"`
}

// EvaluationCounts is what the evaluation did with one arm-and-replicate's
// cells.
type EvaluationCounts struct {
	System string `json:"system"`
	// Finished counts cells whose work is done; Scored, those with a
	// verdict.
	Finished int `json:"finished"`
	Scored   int `json:"scored"`
	Resolved int `json:"resolved"`
	// EmptyPatch counts cells whose arm changed nothing.
	EmptyPatch int `json:"empty_patch"`
	// NotGraded counts cells the harness ran and wrote no report for: a
	// patch that did not apply, a container that did not start, a test run
	// that timed out. They count as unresolved.
	NotGraded int `json:"not_graded"`
	// InfraFailure counts reports the harness itself flagged as a likely
	// infrastructure failure.
	InfraFailure int `json:"infra_failure"`
	// Disagreements counts cells where the port's re-grade disagreed with
	// the official verdict; Unchecked, those it could not re-grade.
	Disagreements int `json:"disagreements"`
	Unchecked     int `json:"unchecked"`
}

// Spend is what a corpus has spent and been charged. See corpus.go, "The
// spend cap".
type Spend struct {
	// CapUSD is the cap the latest run was given, or zero for none.
	CapUSD float64 `json:"cap_usd,omitempty"`
	// RecordedUSD is what finished cells' arms reported spending.
	RecordedUSD float64 `json:"recorded_usd"`
	// ChargedUSD is what interrupted attempts were charged, as an estimate,
	// because nothing reported what they spent.
	ChargedUSD float64 `json:"charged_for_interrupted_attempts_usd"`
	// TotalUSD is the two together: what the cap is checked against.
	TotalUSD float64 `json:"total_usd"`
	// Estimated is true when any part of TotalUSD is an estimate.
	Estimated bool `json:"estimated"`
}

// scoredCell is one cell's result and verdict, as the report reads them.
type scoredCell struct {
	cell    Cell
	result  CellResult
	verdict *Verdict
}

// report builds the corpus's report from every cell on disk, writes it to
// report.json and report.txt in the corpus directory, and returns it.
func (r *corpusRun) report(status string, stop error) (*CorpusReport, error) {
	var cells []scoredCell
	for _, task := range r.opts.Tasks {
		for _, cell := range r.cells(task) {
			dir := r.cellDir(cell)
			if !exists(resultPath(dir)) {
				continue
			}
			sc := scoredCell{cell: cell}
			if err := readJSON(resultPath(dir), &sc.result); err != nil {
				return nil, err
			}
			if exists(verdictPath(dir)) {
				var cv CellVerdict
				if err := readJSON(verdictPath(dir), &cv); err != nil {
					return nil, err
				}
				sc.verdict = &cv.Verdict
			}
			cells = append(cells, sc)
		}
	}

	report, err := buildCorpusReport(r.manifest, r.arms, len(r.opts.Tasks), cells, r.ledger, r.now().UTC())
	if err != nil {
		return nil, err
	}
	report.Status = status
	if status != CorpusComplete && stop != nil {
		report.Warnings = append([]string{"the corpus did not finish: " + firstLine(stop.Error())}, report.Warnings...)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("fixrate: encode the corpus report: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(r.opts.Dir, "report.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(r.opts.Dir, "report.txt"), []byte(report.HumanSummary()), 0o600); err != nil {
		return nil, err
	}
	return report, nil
}

// buildCorpusReport is the report's arithmetic, a pure function of the cells
// it is handed, so its aggregates-only property can be tested without
// running a corpus.
func buildCorpusReport(m CorpusManifest, arms []Arm, tasks int, cells []scoredCell, l corpusLedger, now time.Time) (*CorpusReport, error) {
	report := &CorpusReport{
		SchemaVersion: CorpusReportSchemaVersion,
		Mode:          m.Mode,
		Provenance:    m.Mode.provenance(),
		Corpus:        m.Corpus,
		Scoring:       sweBenchScoring,
		Selection:     m.Selection,
		GeneratedAt:   now,
		Tasks:         tasks,
		Spend: Spend{
			CapUSD:      l.cap,
			RecordedUSD: l.recorded,
			ChargedUSD:  l.charged,
			TotalUSD:    l.total(),
			Estimated:   l.estimated,
		},
	}

	// outcomes[arm][replicate][task]
	outcomes := map[Arm]map[int]map[string]TaskOutcome{}
	counts := map[string]*EvaluationCounts{}
	var systems []string
	tasksSeen := map[string]bool{}
	for _, sc := range cells {
		system := sc.cell.System()
		c, ok := counts[system]
		if !ok {
			c = &EvaluationCounts{System: system}
			counts[system] = c
			systems = append(systems, system)
		}
		c.Finished++
		if sc.cell.Arm == ArmBelay {
			if sc.result.Adapters != "" && !slices.Contains(report.Adapters, sc.result.Adapters) {
				report.Adapters = append(report.Adapters, sc.result.Adapters)
			}
			if sc.cell.Replicate == 1 && sc.result.ShippedGate != "" && !tasksSeen[sc.cell.Task.ID] {
				tasksSeen[sc.cell.Task.ID] = true
				report.ShippedGateRefused++
			}
		}
		if sc.verdict == nil {
			continue
		}
		v := *sc.verdict
		c.Scored++
		switch {
		case v.Resolved:
			c.Resolved++
		case v.EmptyPatch:
			c.EmptyPatch++
		case !v.Evaluated:
			c.NotGraded++
		}
		if v.InfraFailure {
			c.InfraFailure++
		}
		if v.Port.Disagrees {
			c.Disagreements++
		}
		if v.Evaluated && !v.Port.Checked {
			c.Unchecked++
		}
		if outcomes[sc.cell.Arm] == nil {
			outcomes[sc.cell.Arm] = map[int]map[string]TaskOutcome{}
		}
		if outcomes[sc.cell.Arm][sc.cell.Replicate] == nil {
			outcomes[sc.cell.Arm][sc.cell.Replicate] = map[string]TaskOutcome{}
		}
		outcomes[sc.cell.Arm][sc.cell.Replicate][sc.cell.Task.ID] = cellOutcome(sc)
	}
	slices.Sort(systems)
	for _, s := range systems {
		report.Evaluation = append(report.Evaluation, *counts[s])
	}

	// The arms' summaries, and the comparison between them, over their
	// first replicates. With both arms, only tasks both were scored on
	// count, so every number below is over the same tasks.
	first := func(a Arm) map[string]TaskOutcome { return outcomes[a][1] }
	if slices.Contains(arms, ArmBelay) && slices.Contains(arms, ArmControl) {
		var pairs []PairedTask
		for _, id := range m.Tasks {
			b, okB := first(ArmBelay)[id]
			c, okC := first(ArmControl)[id]
			if okB && okC {
				pairs = append(pairs, PairedTask{TaskID: id, Belay: b, Control: c})
			}
		}
		paired, err := summarizePaired(m.Mode, now, pairs)
		if err != nil {
			return nil, err
		}
		report.Paired = len(pairs)
		report.Belay, report.Control, report.Comparison = paired.Belay, paired.Control, paired.Comparison
		report.Warnings = append(report.Warnings, paired.Warnings...)
	} else {
		for _, a := range arms {
			var list []TaskOutcome
			for _, id := range m.Tasks {
				if o, ok := first(a)[id]; ok {
					list = append(list, o)
				}
			}
			summary := summarizeArm(a, list)
			if a == ArmBelay {
				report.Belay = summary
			} else {
				report.Control = summary
			}
		}
	}

	for _, a := range arms {
		if second := outcomes[a][2]; len(second) > 0 {
			report.Replicates = append(report.Replicates, replicateAgreement(a, m.Tasks, first(a), second))
		}
	}
	report.Warnings = append(report.Warnings, evaluationWarnings(report)...)
	return report, nil
}

// cellOutcome turns a scored cell into the TaskOutcome summarizePaired and
// summarizeArm consume. Resolved is the official verdict, and only that.
func cellOutcome(sc scoredCell) TaskOutcome {
	v := *sc.verdict
	reason := "the official harness did not resolve it"
	switch {
	case v.Resolved:
		reason = "the official harness resolved it"
	case v.EmptyPatch:
		reason = "the arm changed nothing"
	case !v.Evaluated:
		reason = "the official harness could not grade it"
	}
	return TaskOutcome{
		TaskID:           sc.cell.Task.ID,
		Arm:              sc.cell.Arm,
		Resolved:         v.Resolved,
		Reason:           reason,
		UsageUSD:         sc.result.UsageUSD,
		UsageEstimated:   sc.result.UsageEstimated,
		Turns:            sc.result.Turns,
		WallClockSeconds: sc.result.WallClockSeconds,
		Model:            sc.result.Model,
		Allowance:        sc.result.Allowance,
	}
}

// replicateAgreement compares an arm's two replicates over the tasks both
// were scored on.
func replicateAgreement(a Arm, order []string, first, second map[string]TaskOutcome) ReplicateAgreement {
	ra := ReplicateAgreement{Arm: a}
	for _, id := range order {
		x, okX := first[id]
		y, okY := second[id]
		if !okX || !okY {
			continue
		}
		ra.Tasks++
		switch {
		case x.Resolved && y.Resolved:
			ra.BothResolved++
		case x.Resolved:
			ra.FirstOnly++
		case y.Resolved:
			ra.SecondOnly++
		default:
			ra.NeitherResolved++
		}
	}
	if ra.Tasks > 0 {
		ra.Agreement = float64(ra.BothResolved+ra.NeitherResolved) / float64(ra.Tasks)
	}
	ra.McNemarExactP = ExactMcNemarP(ra.FirstOnly, ra.SecondOnly)
	return ra
}

// evaluationWarnings says, in words, what a reader must know about the
// evaluation before quoting a number from the report.
func evaluationWarnings(r *CorpusReport) []string {
	var w []string
	for _, c := range r.Evaluation {
		if c.Disagreements > 0 {
			w = append(w, fmt.Sprintf("%s: the official harness and this package's port of its grading DISAGREE on %d cell(s). "+
				"The official verdict is the score; each disagreement is in its cell's verdict.json (port.disagrees) and must be "+
				"read before any number here is quoted", c.System, c.Disagreements))
		}
		if c.Unchecked > 0 {
			w = append(w, fmt.Sprintf("%s: %d scored cell(s) could not be cross-checked; see port.unavailable in their verdict.json",
				c.System, c.Unchecked))
		}
		if c.NotGraded > 0 {
			w = append(w, fmt.Sprintf("%s: the official harness could not grade %d cell(s) -- a patch that did not apply, a "+
				"container that did not start, a test run that timed out -- and they count as unresolved", c.System, c.NotGraded))
		}
		if c.InfraFailure > 0 {
			w = append(w, fmt.Sprintf("%s: the official harness flagged %d cell(s) as likely infrastructure failures", c.System, c.InfraFailure))
		}
		if c.Finished > c.Scored {
			w = append(w, fmt.Sprintf("%s: %d finished cell(s) are not scored yet", c.System, c.Finished-c.Scored))
		}
	}
	if r.Paired < r.Tasks && r.Status == CorpusComplete {
		w = append(w, fmt.Sprintf("only %d of %d task(s) are paired", r.Paired, r.Tasks))
	}
	if r.Spend.ChargedUSD > 0 {
		w = append(w, fmt.Sprintf("$%.2f of the spend is a charge for interrupted attempts whose cost nothing reported; it "+
			"counts toward the cap and is not part of either arm's cost per resolved task", r.Spend.ChargedUSD))
	}
	return w
}

// HumanSummary renders the report for a person, in the order the paired
// report uses: what produced the numbers, then the numbers, cost per
// resolved task beside the rate, then what to know before quoting them.
func (r *CorpusReport) HumanSummary() string {
	var b strings.Builder
	fmt.Fprintln(&b, r.Provenance)
	fmt.Fprintln(&b, "corpus:", r.Corpus)
	fmt.Fprintln(&b, "selection:", r.Selection)
	fmt.Fprintln(&b, r.Scoring)
	for _, a := range r.Adapters {
		fmt.Fprintln(&b, "belay arm adapters:", a)
	}
	fmt.Fprintf(&b, "status: %s; %d task(s), %d paired\n\n", r.Status, r.Tasks, r.Paired)

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
	for _, ra := range r.Replicates {
		fmt.Fprintf(&b, "A/A %s: %d task(s), agreement %.4f, first_only=%d second_only=%d, McNemar p = %.6g\n",
			ra.Arm, ra.Tasks, ra.Agreement, ra.FirstOnly, ra.SecondOnly, ra.McNemarExactP)
	}
	fmt.Fprintln(&b)
	for _, e := range r.Evaluation {
		fmt.Fprintf(&b, "evaluation %s: %d finished, %d scored, %d resolved, %d empty patch, %d not graded, "+
			"%d infra failure, %d disagreement(s), %d unchecked\n", e.System, e.Finished, e.Scored, e.Resolved,
			e.EmptyPatch, e.NotGraded, e.InfraFailure, e.Disagreements, e.Unchecked)
	}
	if r.ShippedGateRefused > 0 {
		fmt.Fprintf(&b, "belay as shipped would have refused %d task(s) for want of a lint gate\n", r.ShippedGateRefused)
	}
	estimated := ""
	if r.Spend.Estimated {
		estimated = " (part estimated)"
	}
	fmt.Fprintf(&b, "spend: $%.4f recorded + $%.4f charged = $%.4f%s", r.Spend.RecordedUSD, r.Spend.ChargedUSD,
		r.Spend.TotalUSD, estimated)
	if r.Spend.CapUSD > 0 {
		fmt.Fprintf(&b, " of a $%.2f cap", r.Spend.CapUSD)
	}
	fmt.Fprintln(&b)
	if len(r.Warnings) > 0 {
		fmt.Fprintln(&b)
		for _, w := range r.Warnings {
			fmt.Fprintln(&b, "warning:", w)
		}
	}
	return b.String()
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
