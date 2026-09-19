package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/detect"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// runFlags holds the settings `belay run` and `belay resume` share. Both
// commands need to know which repository they are working on and where a
// recorded conversation lives, and they answer to the same flag names so a
// person only learns them once.
var runFlags struct {
	cassette string
}

// addRunFlags gives cmd the flags a run needs.
func addRunFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&runFlags.cassette, "cassette", "",
		"the recorded conversation to replay from, or record to (default <workspace>/.belay/cassette.json)")
}

var runCmd = &cobra.Command{
	Use:   `run "<what you want built>"`,
	Short: "Start a run: say what you want built, and belay builds it",
	Long: `Start a run.

Say what you want built, in your own words, as one argument:

    belay run "add table-driven tests for the config parser"

belay writes a plan first and then stops, so you can read the plan — and edit
it — before anything in your repository changes. Continue with "belay resume".
From there belay writes the code, saves the files, runs your tests, fixes what
fails, and puts the result through a quality gate.

belay stops on its own at the budget ceiling set in belay.yaml, and it prints
that ceiling before it spends anything.

Every step is written to disk as it happens, under <workspace>/.belay/runs/.
Press Ctrl-C whenever you like: the run stops, its state stays, and
"belay resume" picks it up from the last completed step.

Use --dry-run to see exactly what a run would do — the settings it read, the
project it found, the tools it would use, and the most it could spend —
without creating anything or spending anything.`,
	Example: `  belay run "add tests for the config parser"
  belay run --workspace ../other-repo "make the CLI accept a --json flag"
  belay --dry-run run "upgrade to the v2 client"`,
	Args: exactlyOneDescription,
	RunE: func(cmd *cobra.Command, args []string) error {
		return startRun(cmd, strings.TrimSpace(args[0]))
	},
}

func init() {
	addRunFlags(runCmd)
	rootCmd.AddCommand(runCmd)
}

// exactlyOneDescription insists on the one thing `belay run` needs, and says
// so in the words a person would use rather than cobra's "accepts 1 arg(s)".
func exactlyOneDescription(_ *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return errors.New(`say what you want built, in quotes: belay run "add tests for the parser"`)
	case len(args) > 1:
		return fmt.Errorf(`belay takes one description, in quotes, but got %d arguments; `+
			`did you forget the quotes around "%s"?`, len(args), strings.Join(args, " "))
	case strings.TrimSpace(args[0]) == "":
		return errors.New("that description is empty; tell belay what you want built")
	default:
		return nil
	}
}

// startRun resolves a run, creates it on disk, and drives it to its end.
func startRun(cmd *cobra.Command, want string) error {
	u := newUI(cmd)

	plan, err := resolve(resolveRequest{
		workspace:  rootFlags.workspace,
		configPath: rootFlags.configPath,
		cassette:   runFlags.cassette,
		logger:     newLogger(u),
	})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	if rootFlags.dryRun {
		return describeDryRun(u, plan, want, "")
	}
	if len(plan.blockers) > 0 {
		return refuseToStart(u, plan)
	}

	runID, err := state.NewRunID()
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	store, err := createRun(plan, runSpec{Workspace: plan.workspace, RunID: runID, Goal: want})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	announceStart(u, plan, store.Layout(), want)
	return driveRun(cmd, u, plan, store, false)
}

// runSpec names the run createRun makes: where it lives, what it is called,
// and what it is for.
//
// The caller chooses the run id, not createRun, so that a caller can record
// the id somewhere of its own before the run exists and find the run again
// after a crash. `belay run` has nowhere to record it and simply asks
// state.NewRunID for one.
type runSpec struct {
	// Workspace is the absolute path of the repository the run works on.
	// The run's directory is made under it, in .belay/runs/.
	Workspace string
	// RunID names the run and its directory.
	RunID string
	// Goal is what the person asked for, exactly as they typed it.
	Goal string
}

// createRun lays down the run directory, its initial state and its manifest,
// in that order. It is the first thing in `belay run` that writes anything at
// all; everything before it is resolution, which is what lets --dry-run stop
// here.
//
// The manifest goes last because writing it is what creates the run.
// Everything that looks for runs reads manifest.json and nothing else: belay
// resume chooses from the manifests it finds, and belay runs lists them. The
// dispatcher, for its part, cannot continue a run without state.json. When
// the manifest came first, a failure or a crash before state.json was written
// left a manifest saying "running" over a run with no state: resume chose it,
// and the dispatcher failed to load it. Now a failure or a crash anywhere
// before the manifest leaves a directory without one, which resume never
// chooses and belay runs reports as unreadable.
//
// createRun does not delete that directory when it fails. A crash leaves the
// same directory with no chance to delete anything, so every reader must
// already treat a directory without a manifest as not a run; deleting it on
// the error path would add a way to remove files without sparing any reader
// that work. Calling createRun again for the same run id converges instead:
// it writes the state again, whole, then the manifest. It must not be called
// for a run whose manifest exists, which it would start over.
func createRun(plan *runPlan, spec runSpec) (*state.Store, error) {
	layout, err := state.NewLayout(spec.Workspace, spec.RunID)
	if err != nil {
		return nil, err
	}
	// Creating artifacts/ creates the run directory above it in one call.
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		return nil, fmt.Errorf("cannot create the run directory %s: %w", layout.RunDir(), err)
	}

	if err := state.SaveState(layout.StatePath(), state.NewState(spec.Goal)); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", layout.StatePath(), err)
	}
	now := time.Now().UTC()
	manifest := state.NewManifest(now, spec.RunID, spec.Workspace, spec.Goal, plan.config)
	if err := plan.prepareWorkTree(layout, &manifest); err != nil {
		return nil, err
	}
	if err := state.SaveManifest(layout.ManifestPath(), manifest); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", layout.ManifestPath(), err)
	}
	return state.NewStore(layout), nil
}

// prepareWorkTree readies the tree a new run will change, and records in the
// run's manifest whatever a later resume must know about that tree. createRun
// calls it once, after the run's state is written and before its manifest is.
//
// Today it does nothing. Every run changes the workspace it was started in,
// so there is no tree to make and nothing to record. The step exists so that
// a run which works somewhere else, such as a worktree of its own, has one
// place to make that tree before the manifest pins it, instead of a new write
// wedged in among createRun's.
//
// What it grows to do must keep to the order around it. It runs after
// state.json, so a failure here leaves a directory with no manifest, which
// nothing takes for a run. It runs before the manifest, so what it records
// lands in the manifest's one write. And a failure after it, writing the
// manifest, cannot undo what it made outside the run directory, so it must be
// safe to repeat when createRun is called again for the same run id.
//
//nolint:revive // layout and manifest are unused until a run can work outside its workspace; they are the step's contract.
func (p *runPlan) prepareWorkTree(layout state.Layout, manifest *state.Manifest) error {
	return nil
}

// driveRun runs the dispatcher over an existing run and reports how it ended.
// `belay run` and `belay resume` differ only in the resuming flag, so the two
// commands cannot narrate the same events differently.
func driveRun(cmd *cobra.Command, u *ui, plan *runPlan, store *state.Store, resuming bool) error {
	dispatcher, err := graph.NewDispatcher(plan.options(store))
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}

	ctx, stop := stopOnSignal(cmd.Context(), u)
	defer stop()

	// Run and Resume differ in exactly one thing: whether a run parked at
	// an approval gate may be stepped past. Calling both would step past a
	// gate that a plain `belay run` had just been told to respect.
	dispatch := dispatcher.Run
	if resuming {
		dispatch = dispatcher.Resume
	}
	outcome, runErr := dispatch(ctx)

	if path, err := plan.saveCassette(); err != nil {
		u.note("belay: %v", err)
	} else if path != "" {
		u.say("  recording saved to %s", path)
	}

	return reportOutcome(u, outcomeView{layout: store.Layout(), outcome: outcome, runErr: runErr})
}

// stopOnSignal cancels ctx when the person running belay asks it to stop, and
// says on screen that stopping is safe.
//
// The first SIGINT or SIGTERM cancels the run's context: the dispatcher
// finishes the durable write it is in the middle of, journals an abort and
// returns, so the run stops with its state intact. signal.Stop then hands the
// next signal back to Go's default handler, which ends the process at once —
// somebody holding Ctrl-C must never be left wondering whether belay is
// ignoring them.
func stopOnSignal(parent context.Context, u *ui) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ch:
			signal.Stop(ch)
			u.say("")
			u.say("Stopping. belay saves its state after every step, so nothing is lost.")
			u.say("Press Ctrl-C again to quit immediately.")
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(ch)
		cancel()
		<-done
	}
}

// announceStart prints what a person needs before belay spends anything: what
// it was asked for, what it may cost, how to stop it, and where its state
// lives if they do.
func announceStart(u *ui, plan *runPlan, layout state.Layout, want string) {
	u.say("belay is starting a run in %s", plan.workspace)
	u.say("  what you asked for:  %s", want)
	u.say("  most it can spend:   %s", u.emph(budgetCeiling(plan.config.Budget)))
	u.say("  when it gets there:  %s", budgetOnExceed(plan.config.Budget))
	u.say("  to stop it:          press Ctrl-C once; belay saves its state and exits")
	u.say("")
	u.say("run %s", layout.RunID())
	u.say("  state:  %s", layout.RunDir())
	u.say("  route:  %s", routeLine(plan.route))
	u.say("")
}

// outcomeView is everything belay's closing report reads about how one
// dispatch of a run ended: which run it was, what the dispatcher returned,
// and the error it gave as the reason a run stopped short.
//
// It is one value rather than a list of parameters so that the report can
// learn something new about a run, such as where the run's work went, by
// gaining a field, instead of by changing the signature of every function
// that reads it.
type outcomeView struct {
	// layout locates the run's files. The report names the plan, the
	// journal and the command that continues the run.
	layout state.Layout
	// outcome is what the dispatcher returned.
	outcome graph.Outcome
	// runErr is the dispatcher's error. It is part of how the run went, such
	// as graph.ErrMaxSteps, not a failure to find out.
	runErr error
}

// reportOutcome turns an Outcome into the last thing a person reads, and into the
// process's exit status.
//
// Every branch answers the same three questions in the same order: where the
// run is, what it cost, and what to do next. A person who has read one of
// these has read all of them.
func reportOutcome(u *ui, view outcomeView) error {
	outcome, runErr := view.outcome, view.runErr
	u.say("")
	u.say("%s", outcomeHeadline(outcome, runErr))
	u.say("  where:  %s", whereText(outcome.Node))
	u.say("  cost:   %s", u.emph(spendText(outcome)))
	// The label sits on the first line only: repeating "next:" down a
	// paragraph turns one instruction into four, which is exactly the
	// reading a person then has to undo.
	for i, line := range nextSteps(view) {
		switch {
		case line == "":
			u.say("")
		case i == 0:
			u.say("  next:   %s", line)
		default:
			u.say("          %s", line)
		}
	}
	if note := strings.TrimSpace(outcome.Note); note != "" && u.verbose {
		u.say("  note:   %s", note)
	}

	if outcome.ExitCode == graph.ExitOK {
		return nil
	}
	return &ExitError{Code: outcome.ExitCode, Err: runErr}
}

// outcomeHeadline states, in one sentence, how this invocation ended.
func outcomeHeadline(outcome graph.Outcome, runErr error) string {
	steps := "step"
	if outcome.Steps != 1 {
		steps = "steps"
	}
	did := fmt.Sprintf("after %d %s", outcome.Steps, steps)

	switch {
	case errors.Is(runErr, graph.ErrRunPaused):
		return "This run is waiting for you, so belay did nothing."
	case errors.Is(runErr, graph.ErrRunEnded):
		return "This run has already ended, so belay did nothing."
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, graph.ErrNodeAborted) && outcome.Steps == 0:
		return "belay stopped because you asked it to. Everything it had done is saved."
	}

	switch outcome.Status {
	case state.RunStatusCompleted:
		return fmt.Sprintf("belay finished %s.", did)
	case state.RunStatusPaused:
		return fmt.Sprintf("belay paused %s, and is waiting for you.", did)
	case state.RunStatusFailed:
		return fmt.Sprintf("belay could not finish; it stopped %s. Everything it did is saved.", did)
	case state.RunStatusAborted:
		return fmt.Sprintf("belay stopped itself %s. Everything it did is saved.", did)
	default:
		return fmt.Sprintf("belay stopped %s.", did)
	}
}

// whereText explains a node name rather than printing it bare. A person who has
// never read belay's source has no way to know what "approve" means, and a
// message they cannot act on is worse than no message.
func whereText(node string) string {
	switch node {
	case "":
		return "belay had not reached a step yet"
	case graph.End:
		return "the end of the graph — there is nothing left to do"
	default:
		if meaning, ok := nodeMeaning[node]; ok {
			return node + " — " + meaning
		}
		return node
	}
}

// nodeMeaning says what each step of the graph is, in one clause.
var nodeMeaning = map[string]string{
	graph.NodePlan:    "writing the plan",
	graph.NodeApprove: "waiting for you to read the plan and approve it",
	graph.NodeCode:    "asking the agent to write the change",
	graph.NodeWrite:   "saving the agent's changes into your repository",
	graph.NodeTest:    "running your tests",
	graph.NodeFix:     "asking the agent to repair what failed",
	graph.NodeReview:  "running the quality gate over the result",
}

// spendText renders what the run has cost so far against what it was allowed.
func spendText(outcome graph.Outcome) string {
	used := dollars(outcome.Usage.USD)
	if outcome.Usage.Estimated {
		used += " (estimated)"
	}
	tokens := outcome.Usage.InputTokens + outcome.Usage.OutputTokens
	if tokens == 0 {
		return used
	}
	return fmt.Sprintf("%s, %s tokens", used, thousands(tokens))
}

// nextSteps is the wayfinding line: the exact command that continues from
// here, written out so nobody has to remember or reconstruct it.
func nextSteps(view outcomeView) []string {
	layout, outcome, runErr := view.layout, view.outcome, view.runErr
	resume := "belay resume " + layout.RunID()

	switch {
	case errors.Is(runErr, graph.ErrRunPaused), outcome.Status == state.RunStatusPaused:
		lines := []string{"read the plan, edit it if you want to, then run:", "", "    " + resume}
		if outcome.Node == graph.NodeApprove {
			plan := filepath.Join(layout.ArtifactsDir(), "plan.md")
			lines = append([]string{"the plan is at " + plan}, lines...)
		}
		return lines
	case errors.Is(runErr, graph.ErrNoResumePoint):
		return []string{
			"belay cannot tell where this run left off, so it will not guess.",
			"start a fresh run; the journal of this one is at " + layout.JournalPath(),
		}
	case errors.Is(runErr, graph.ErrMaxSteps):
		return []string{
			"the graph kept going round without finishing, so belay stopped it.",
			"raise graph.max_steps in belay.yaml, or take a smaller bite:",
			"", "    " + resume,
		}
	case errors.Is(runErr, graph.ErrNodeTimeout):
		return []string{
			"one step ran longer than graph.node_timeout allows.",
			"raise it in belay.yaml, then:", "", "    " + resume,
		}
	case outcome.Status == state.RunStatusCompleted:
		return []string{"review the change in your repository, then commit it"}
	case outcome.Status == state.RunStatusAborted:
		return []string{"pick up where it stopped with:", "", "    " + resume}
	case outcome.Status == state.RunStatusFailed:
		return []string{
			"see what happened with:", "", "    belay timeline " + layout.RunID(),
		}
	default:
		return []string{"continue with:", "", "    " + resume}
	}
}

// refuseToStart reports every reason belay will not start, together, and stops.
func refuseToStart(u *ui, plan *runPlan) error {
	u.say("belay will not start this run.")
	for _, b := range plan.blockers {
		u.say("")
		u.say("  %s", b.problem)
		u.say("  to fix it: %s", b.fix)
	}
	u.say("")
	u.say("Nothing was created and nothing was spent.")
	return &ExitError{Code: graph.ExitFailed}
}

// describeDryRun prints the whole of what a run would do and then stops,
// having created nothing and spent nothing.
//
// It reads from the same resolved plan a real run would use, so it cannot
// describe a run different from the one that would happen. resuming, when not
// empty, names the run this would have continued.
func describeDryRun(u *ui, plan *runPlan, want, resuming string) error {
	u.say("This is a dry run. belay is only describing the work; none of it has happened.")
	u.say("")

	if want != "" {
		u.say("what you asked for")
		u.say("  %s", want)
		u.say("")
	}
	if resuming != "" {
		u.say("which run it would continue")
		u.say("  %s", resuming)
		u.say("")
	}

	u.say("where belay would work")
	u.say("  %s", plan.workspace)
	u.say("  %s", projectLine(plan.projects))
	u.say("")

	u.say("the most it could spend")
	u.say("  %s", u.emph(budgetCeiling(plan.config.Budget)))
	u.say("  %s", budgetOnExceed(plan.config.Budget))
	u.say("")

	u.say("settings it would use")
	u.say("  read from    %s", sourcesLine(plan.configSources))
	u.say("  agent        %s, model %s, %s, up to %d turns per step",
		plan.config.Agent.Backend, plan.config.Agent.Model,
		agentModeLine(plan.config.Agent.Mode, plan.cassettePath), plan.config.Agent.MaxTurns)
	u.say("  tests        %s", plan.runnerName())
	u.say("  quality gate %s, failing the run on %s issues or worse",
		plan.gateName(), plan.config.Review.FailOn)
	u.say("  credentials  %s", credentialsLine())
	u.say("")

	u.say("the route it would take")
	u.say("  %s", routeLine(plan.route))
	u.say("  %s", approvalLine(plan.config.Graph))
	u.say("  it gives up after %d rounds of the test-and-fix loop, or %d steps in all",
		plan.config.Graph.GiveUp, plan.config.Graph.MaxSteps)
	u.say("")

	u.say("what it would create")
	u.say("  %s", filepath.Join(plan.workspace, ".belay", "runs", "<run-id>"))
	u.say("  holding manifest.json, state.json, journal.ndjson and the plan")
	u.say("")

	for _, w := range plan.warnings {
		u.say("worth knowing")
		u.say("  %s", w)
		u.say("")
	}

	if len(plan.blockers) > 0 {
		u.say("belay would refuse to start this run.")
		for _, b := range plan.blockers {
			u.say("")
			u.say("  %s", b.problem)
			u.say("  to fix it: %s", b.fix)
		}
		u.say("")
		u.say("Nothing was created and nothing was spent.")
		return &ExitError{Code: graph.ExitFailed}
	}

	u.say("Nothing was created and nothing was spent. Run the same command without --dry-run to start.")
	return nil
}

// budgetCeiling renders the budget in the two units it is capped in.
func budgetCeiling(b config.Budget) string {
	return fmt.Sprintf("%s or %s tokens, whichever comes first", dollars(b.MaxUSD), thousands(b.MaxTokens))
}

// budgetOnExceed says what reaching the ceiling actually does, because "abort" and
// "warn" are the difference between a cap and a suggestion.
func budgetOnExceed(b config.Budget) string {
	if b.OnExceed == config.OnExceedWarn {
		return "belay records the overrun and keeps going — set budget.on_exceed to \"abort\" to make it a hard stop"
	}
	return "belay stops the run before the next step"
}

// routeLine is the walk through the graph, written out so a person can see
// where they are in it. route is what nodes.Route returned for this run.
func routeLine(route []string) string {
	return strings.Join(append(slices.Clone(route), "done"), " → ")
}

// approvalLine explains the one step that stops for a human.
func approvalLine(g config.Graph) string {
	if g.Approval {
		return "approve stops the run so you can read the plan before any code is written"
	}
	return "graph.approval is false, so belay will not stop for you: it goes from plan straight to code"
}

// projectLine names what belay recognised in the workspace.
func projectLine(projects []detect.Project) string {
	if len(projects) == 0 {
		return "belay recognises no Go, Node or Python project here"
	}
	// Count kinds rather than listing one entry per detected module. A
	// repository with nested modules -- belay's own, with two fixture
	// modules under it -- otherwise renders as "go, go, go", which repeats a
	// word three times and tells the reader nothing they can act on.
	counts := make(map[string]int, len(projects))
	order := make([]string, 0, len(projects))
	for _, p := range projects {
		k := p.Kind.String()
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	kinds := make([]string, 0, len(order))
	for _, k := range order {
		if n := counts[k]; n > 1 {
			kinds = append(kinds, fmt.Sprintf("%s (%d modules)", k, n))
			continue
		}
		kinds = append(kinds, k)
	}
	return "belay found: " + strings.Join(kinds, ", ")
}

// sourcesLine names the configuration files belay read.
func sourcesLine(sources []string) string {
	if len(sources) == 0 {
		return "belay's built-in defaults (no belay.yaml found)"
	}
	return strings.Join(sources, ", then ")
}

// agentModeLine says what agent.mode means in terms of what will happen.
func agentModeLine(mode config.AgentMode, cassette string) string {
	switch mode {
	case config.AgentModeReplay:
		return "replaying the recording at " + cassette + " (it will not call the agent at all)"
	case config.AgentModeRecord:
		return "calling the agent for real and recording it to " + cassette
	default:
		return "calling the agent for real"
	}
}

// credentialsLine reports whether each credential belay reads is set. It never
// reports their values.
func credentialsLine() string {
	names := []string{anthropicKeyEnv, claudeOAuthTokenEnv, sonarTokenEnv}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if os.Getenv(name) == "" {
			parts = append(parts, name+" is not set")
			continue
		}
		parts = append(parts, name+" is set")
	}
	return strings.Join(parts, "; ")
}

// dollars renders an amount the way a person reads money.
func dollars(v float64) string { return fmt.Sprintf("$%.2f", v) }

// thousands groups a number's digits in threes. A budget ceiling is not a place
// to make somebody count zeroes: 2000000 and 20000000 are the same shape at a
// glance, and 2,000,000 and 20,000,000 are not.
func thousands(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(digits[i])
	}
	return sign + b.String()
}

// newLogger returns the logger a run's dispatcher and adapters write to: the
// narrator, which renders the two events a person cares about and, with
// --verbose, forwards everything to stderr as well.
func newLogger(u *ui) *slog.Logger {
	var next slog.Handler
	if u.verbose {
		next = slog.NewTextHandler(u.err, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	return slog.New(&narrator{ui: u, next: next})
}

// Messages the dispatcher logs as it walks the graph. The narrator turns
// exactly these two into the run's on-screen story; everything else is
// belay talking to its operator, not to the person waiting.
const (
	msgNodeStarted  = "belay/graph: node started"
	msgNodeFinished = "belay/graph: node finished"
)

// nodeColumn is the width the node names are padded to, so the story reads
// down a column instead of zig-zagging. graph's longest node name is seven
// characters; the extra two are the gap.
const nodeColumn = 9

// narrator renders the dispatcher's structured log as the run's on-screen
// story.
//
// The dispatcher is the only thing that knows which node it is entering and
// which way it left, and it publishes that through its slog.Logger. Reading
// it here — rather than making the dispatcher print — keeps the graph free of
// a terminal, and keeps the wayfinding in the layer that owns the words.
type narrator struct {
	ui      *ui
	attrs   []slog.Attr
	grouped bool
	next    slog.Handler
}

var _ slog.Handler = (*narrator)(nil)

// Enabled implements slog.Handler.
func (n *narrator) Enabled(ctx context.Context, level slog.Level) bool {
	if n.next != nil {
		return n.next.Enabled(ctx, level)
	}
	return level >= slog.LevelInfo
}

// Handle implements slog.Handler. It narrates the two node events and passes
// every record on to the verbose log when there is one.
func (n *narrator) Handle(ctx context.Context, rec slog.Record) error {
	if !n.grouped {
		switch rec.Message {
		case msgNodeStarted:
			n.entered(n.fields(rec))
		case msgNodeFinished:
			n.left(n.fields(rec))
		}
	}
	if n.next != nil {
		return n.next.Handle(ctx, rec)
	}
	return nil
}

// WithAttrs implements slog.Handler. The dispatcher attaches node, step and
// attempt this way, so they have to be accumulated rather than read off the
// record alone.
func (n *narrator) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := n.clone()
	c.attrs = append(slices.Clip(c.attrs), attrs...)
	if c.next != nil {
		c.next = c.next.WithAttrs(attrs)
	}
	return c
}

// WithGroup implements slog.Handler. Grouped attributes are namespaced, so a
// narrator inside a group stops reading them and only forwards.
func (n *narrator) WithGroup(name string) slog.Handler {
	if name == "" {
		return n
	}
	c := n.clone()
	c.grouped = true
	if c.next != nil {
		c.next = c.next.WithGroup(name)
	}
	return c
}

func (n *narrator) clone() *narrator {
	c := *n
	return &c
}

// nodeEvent is one node transition, as the narrator reads it off the log.
type nodeEvent struct {
	node    string
	step    int64
	attempt int64
	status  string
	next    string
	elapsed time.Duration
}

// fields collects the attributes the dispatcher attached, from the handler's
// accumulated set and from the record itself.
func (n *narrator) fields(rec slog.Record) nodeEvent {
	var ev nodeEvent
	read := func(a slog.Attr) {
		switch a.Key {
		case "node":
			ev.node = a.Value.String()
		case "step":
			ev.step = a.Value.Int64()
		case "attempt":
			ev.attempt = a.Value.Int64()
		case "status":
			ev.status = a.Value.String()
		case "next":
			ev.next = a.Value.String()
		case "elapsed":
			ev.elapsed = a.Value.Duration()
		}
	}
	for _, a := range n.attrs {
		read(a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		read(a)
		return true
	})
	return ev
}

// entered prints the line that answers "what is belay doing now". It is the
// only line in a run that is emphasised, along with the closing cost.
func (n *narrator) entered(ev nodeEvent) {
	line := fmt.Sprintf("  → %s step %d", padNode(ev.node), ev.step)
	if ev.attempt > 1 {
		line += fmt.Sprintf(", attempt %d", ev.attempt)
	}
	n.ui.say("%s", n.ui.emph(line))
}

// left prints the line that answers "which way did it go". Naming the route
// out of a node is what lets somebody follow a loop — test back to fix, fix
// back to test — instead of watching the same names scroll past.
func (n *narrator) left(ev nodeEvent) {
	n.ui.say("    %s %s", padNode(ev.node), nodeVerdict(ev))
}

// nodeVerdict renders how a node ended and where the run went next.
func nodeVerdict(ev nodeEvent) string {
	took := "took " + ev.elapsed.Round(time.Millisecond).String()
	switch ev.status {
	case "ok":
		if ev.next == graph.End {
			return took + " → done"
		}
		return took + " → " + ev.next
	case "paused":
		return took + " → paused, waiting for you"
	case "aborted":
		return took + " → stopped here"
	case "failed":
		return took + " → failed, the run stops here"
	default:
		return took
	}
}

// padNode lines the node names up into a column.
func padNode(node string) string {
	if len(node) >= nodeColumn {
		return node
	}
	return node + strings.Repeat(" ", nodeColumn-len(node))
}
