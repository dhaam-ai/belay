package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/fork"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/isolate/dircopy"
	"github.com/dhaam-ai/belay/internal/state"
)

// forkFlags holds the settings belonging to `belay fork` alone.
var forkFlags struct {
	visit        int
	model        string
	currentFiles bool
}

var forkCmd = &cobra.Command{
	Use:   "fork <run-id> <node>",
	Short: "Re-run one step of a run in a copy, without paying again for the steps before it",
	Long: `Re-run one step of an existing run, from the state the run was in just
before that step:

    belay fork 20260905T153301Z-1f2e3d4c5b6a fix

belay makes a new run, with an id of its own, in a copy of your repository
under .belay/forks/<fork-id>. The new run inherits what the original did
before that step, so none of it is run, or paid for, again. It runs the one
step, then pauses. The original run is only ever read: its journal, its
state and its files stay exactly as they were.

--visit picks which time the step ran, numbered the way "belay timeline"
numbers them: "fix #2" there is --visit 2 here. Without it, belay re-runs the
last one.

--model sends the step a model of your choosing. It applies to the steps that
ask a model for something: plan, code and fix.

belay checks that your files are the ones the step saw. If they have changed
since, because a later step edited them or you did, it stops and says so:
re-running the step on different files is a different experiment. Pass
--current-files to re-run it on the files as they are now.

The agent's conversation does not carry over. The original's went on past this
step, so the fork starts a new one.

The fork edits only its copy, and commits nothing. When it pauses, belay says
how to see what the step did and how to carry the fork on through the rest of
the graph, which is an ordinary resume of the new run, in its copy:

    belay --workspace .belay/forks/<fork-id> timeline <fork-id>
    belay --workspace .belay/forks/<fork-id> resume <fork-id>

fanout, join and promote cannot be forked, nor can a run recorded by a belay
that kept no snapshots of its steps.`,
	Example: `  belay fork 20260905T153301Z-1f2e3d4c5b6a fix
  belay fork 20260905T153301Z-1f2e3d4c5b6a fix --visit 2 --model opus
  belay fork 20260905T153301Z-1f2e3d4c5b6a test --current-files
  belay --dry-run fork 20260905T153301Z-1f2e3d4c5b6a code`,
	Args: aRunAndANode,
	RunE: func(cmd *cobra.Command, args []string) error {
		return startFork(cmd, forkRequest{
			runID: strings.TrimSpace(args[0]), node: strings.TrimSpace(args[1]),
			visit: forkFlags.visit, model: strings.TrimSpace(forkFlags.model),
			currentFiles: forkFlags.currentFiles, dryRun: rootFlags.dryRun,
		})
	},
}

func init() {
	f := forkCmd.Flags()
	f.IntVar(&forkFlags.visit, "visit", 0,
		"which time the step ran, numbered as belay timeline numbers them (default: the last)")
	f.StringVar(&forkFlags.model, "model", "",
		"the model the step is sent this time; for plan, code and fix")
	f.BoolVar(&forkFlags.currentFiles, "current-files", false,
		"re-run the step on the files as they are now, even if they have changed since it ran")
	addRunFlags(forkCmd)
	rootCmd.AddCommand(forkCmd)
}

// aRunAndANode insists on the two things `belay fork` needs, and says how to
// find the second, because nobody remembers which steps a run took.
func aRunAndANode(_ *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return errors.New("name the run, and the step of it to re-run: belay fork <run-id> <node>; " +
			"belay runs lists the runs")
	case len(args) == 1:
		return fmt.Errorf("name the step of run %s to re-run as well: belay fork %s <node>; "+
			"belay timeline %s lists the steps it ran", args[0], args[0], args[0])
	case len(args) > 2:
		return fmt.Errorf("belay fork takes a run id and a step, but got %d arguments", len(args))
	case strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "":
		return errors.New("the run id and the step must not be empty: belay fork <run-id> <node>")
	default:
		return nil
	}
}

// forkRequest is what a person asked `belay fork` for.
type forkRequest struct {
	runID, node  string
	visit        int
	model        string
	currentFiles bool
	dryRun       bool
}

// check refuses a request that makes no sense whatever the run holds, before
// anything is read.
func (r forkRequest) check() error {
	if r.visit < 0 {
		return errors.New("--visit counts from 1, the way belay timeline numbers a step's visits; " +
			"leave it out to re-run the last")
	}
	if routable := config.RoutableNodes(); r.model != "" && !slices.Contains(routable, r.node) {
		return fmt.Errorf("%s asks no model for anything, so --model would change nothing it does; "+
			"--model applies to %s", r.node, strings.Join(routable, ", "))
	}
	return nil
}

// configure is the configuration a fork runs under: belay.yaml, except that a
// fork always edits its copy in place and files no plan, and its node is sent
// the model --model names.
//
// Output and archival are set here, before resolve decides anything from
// them, rather than patched onto the plan afterwards: resolveOutput would
// otherwise choose a branch, or refuse for want of one, for a run that will
// never have one. The manifest records them as they are, so what a fork was
// started with says what it does.
func (r forkRequest) configure(cfg *config.Config) {
	cfg.Output.Mode = config.OutputInPlace
	cfg.Plan.Archive = false
	if r.model != "" {
		models := maps.Clone(cfg.Agent.Models)
		if models == nil {
			models = config.Models{}
		}
		models[r.node] = r.model
		cfg.Agent.Models = models
	}
}

// startFork resolves the configuration a fork runs under, and makes it.
func startFork(cmd *cobra.Command, req forkRequest) error {
	u := newUI(cmd)
	if err := req.check(); err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	plan, err := resolve(resolveRequest{
		workspace:  rootFlags.workspace,
		configPath: rootFlags.configPath,
		cassette:   runFlags.cassette,
		logger:     newLogger(u),
		context:    cmd.Context(),
		override:   req.configure,
	})
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	return makeFork(cmd, u, plan, req)
}

// makeFork is the rest of `belay fork`, given the plan it runs under: it
// decides the fork (internal/fork's Prepare), says what it decided, makes it,
// runs its one node and reports.
//
// The source run is read and never written, here as in internal/fork: it is
// found with the same picker `belay plan` uses for a finished run, its
// manifest is loaded, and its plan is read wherever the run filed it.
func makeFork(cmd *cobra.Command, u *ui, plan *runPlan, req forkRequest) error {
	ctx := cmd.Context()
	chosen, err := choosePlanRun(plan.workspace, req.runID)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: err.Error(), Err: err}
	}
	source, err := state.NewLayout(plan.workspace, chosen.id)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	man, err := state.LoadManifest(source.ManifestPath())
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: cannot read run " + chosen.id + ": " + err.Error(), Err: err}
	}
	tree, treeNote := sourceTree(source, man)

	spec, err := fork.Prepare(ctx, fork.Request{
		Source: source, Tree: tree, Node: req.node, Visit: req.visit,
		AcceptCurrentFiles: req.currentFiles, Config: plan.config, Survey: surveyWorkspace,
		Plan: func() ([]byte, error) { return sourcePlan(ctx, u, plan.workspace, source, man) },
	})
	var refusal *fork.Refusal
	switch {
	case errors.As(err, &refusal):
		plan.block(refusal.Reason, forkFix(refusal.Err, chosen.id))
	case err != nil:
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	// The guardrails a fork is held to are its source's, as a resume's are,
	// whatever belay.yaml says now: the baseline they judge against is the
	// source's, taken before its change began.
	plan.bindPolicy(man)
	// And with nothing to tell its change from where the source began: a
	// fork's gate judges the agent's report, and says so (fork.Unrecorded).
	plan.changes = &state.Changes{From: state.ChangesReported, Why: fork.Unrecorded}
	plan.changesRepo = nil
	plan.goal = man.Goal

	if req.dryRun {
		return describeFork(u, plan, chosen, spec, treeNote)
	}
	if len(plan.blockers) > 0 {
		return refuseToFork(u, plan)
	}

	announceFork(u, plan, chosen, spec, treeNote)
	f, err := createFork(ctx, plan, spec)
	if err != nil {
		if errors.As(err, &refusal) {
			plan.block(refusal.Reason, forkFix(refusal.Err, chosen.id))
			return refuseToFork(u, plan)
		}
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	u.say("fork %s", f.Layout.RunID())
	u.say("  files:  %s", f.Workspace.Dir)
	u.say("  state:  %s", f.Layout.RunDir())
	u.say("")

	// A recording belongs to the run that made it. Left at the default, it
	// would be saved over the source workspace's own, which may be what the
	// source run was recorded into.
	if plan.recorder != nil && runFlags.cassette == "" {
		plan.cassettePath = cassettePath(f.Workspace.Dir, "")
	}
	store, err := plan.workStore(f.Layout)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	return driveFork(cmd, u, plan, store, f.Workspace.Dir)
}

// createFork makes the fork spec describes as a new run, in a copy under the
// workspace's .belay/forks.
func createFork(ctx context.Context, plan *runPlan, spec *fork.Spec) (fork.Fork, error) {
	id, err := state.NewRunID()
	if err != nil {
		return fork.Fork{}, err
	}
	root, err := state.ForksDir(plan.workspace)
	if err != nil {
		return fork.Fork{}, err
	}
	iso, err := dircopy.New(root, dircopy.WithLogger(plan.logger))
	if err != nil {
		return fork.Fork{}, err
	}
	return spec.Create(ctx, id, iso, time.Now())
}

// driveFork dispatches a new fork once, stopping it after its one node, does
// what follows every dispatch, and reports.
//
// It is driveRun for a fork, with two differences. The dispatch stops after
// one node (graph.Options.PauseAfter), which is the whole point of a fork. And
// a gate that pauses is not offered for review here: the fork was asked to
// run one step, and the person reads the result first. Ctrl-C, the closing
// steps and the report are driveRun's, so the two cannot drift apart.
func driveFork(cmd *cobra.Command, u *ui, plan *runPlan, store *state.Store, copyDir string) error {
	ctx, stop := stopOnSignal(cmd.Context(), u)
	defer stop()

	view, err := dispatchOnce(ctx, plan, store, false, pauseOnCancel, pauseAfterOneNode)
	if err != nil {
		return &ExitError{Code: graph.ExitFailed, Message: "belay: " + err.Error(), Err: err}
	}
	view.workspace = copyDir
	view.pausedAsAsked = pausedAfterItsNode(view)
	// As driveRun: the closing steps run under the command's context, which
	// a Ctrl-C during the dispatch has not cancelled.
	afterCompletion(cmd.Context(), u, plan, &view)
	return reportOutcome(u, view)
}

// pauseAfterOneNode is the change a fork makes to the dispatcher's Options:
// run one node, then pause.
func pauseAfterOneNode(o *graph.Options) { o.PauseAfter = 1 }

// pausedAfterItsNode reports whether a fork's dispatch paused because it was
// asked to stop after one node, rather than because the node itself paused —
// the gate stopping to ask about the plan — or one of the dispatcher's own
// limits stopped it.
//
// The limits and a Ctrl-C all report a cause; the gate does not, so the
// journal is asked whether the last thing the run did was the gate pausing,
// which is the rule everything that treats a person as having been asked
// shares (gateAsked).
func pausedAfterItsNode(view outcomeView) bool {
	if view.runErr != nil || view.outcome.Status != state.RunStatusPaused {
		return false
	}
	asked, err := gateAsked(view.layout)
	return err == nil && !asked
}

// sourceTree returns the directory the source run's nodes worked in, which is
// what a fork copies and compares: the workspace, for a run that edited it in
// place, or the run's own worktree, for a run on a branch.
//
// A branch run's worktree is removed once the run completes, and then the
// checkout the run started from is the only tree there is. note says so: its
// files are not the ones the run worked on, so they will not match the
// fingerprint, and only --current-files will run the fork on them.
func sourceTree(layout state.Layout, man state.Manifest) (tree, note string) {
	if !man.Output.Branched() {
		return layout.WorkspaceDir(), ""
	}
	if wt := workTreeFor(layout, man.Output); isDir(wt) {
		return wt, ""
	}
	return layout.WorkspaceDir(), fmt.Sprintf("run %s worked in its own worktree, which is gone now that the run "+
		"is over; the only files belay can copy are your checkout's, at %s", man.RunID, layout.WorkspaceDir())
}

// sourcePlan reads the plan the source run worked from, wherever it is now:
// in the run's artifacts while the run is under way, and in the repository
// once a finished run has filed it. It is finishedPlan, the rule `belay plan`
// reads a finished run's plan by.
func sourcePlan(ctx context.Context, u *ui, workspace string, layout state.Layout, man state.Manifest) ([]byte, error) {
	st, err := state.LoadState(layout.StatePath())
	if err != nil {
		return nil, err
	}
	body, _, err := finishedPlan(ctx, u, workspace, layout, man, st)
	return body, err
}

// forkFix is the change that clears a refusal, for the person reading it.
func forkFix(kind error, runID string) string {
	switch {
	case errors.Is(kind, fork.ErrFilesChanged):
		return "pass --current-files to re-run it on the files as they are now, or put them back the way they were"
	case errors.Is(kind, fork.ErrNoSuchVisit):
		return "see which steps run " + runID + " ran, and how many times, with: belay timeline " + runID
	case errors.Is(kind, fork.ErrUnsupportedNode):
		return "fork a step before it, or start a new run"
	case errors.Is(kind, fork.ErrSourceActive):
		return "wait for it to stop, or finish it first with: belay resume " + runID
	case errors.Is(kind, fork.ErrPlanChanged):
		return "fork approve instead, so the gate judges the plan as it is now, or put the plan back as it was approved"
	case errors.Is(kind, fork.ErrNoPlan):
		return "put the plan back where the run left it, or fork a step after code, which does not read it"
	default:
		return "start a new run: belay run \"what you want built\""
	}
}

// refuseToFork reports every reason belay will not make a fork, together, and
// stops.
func refuseToFork(u *ui, plan *runPlan) error {
	u.say("belay will not fork this run.")
	for _, b := range plan.blockers {
		u.say("")
		u.say("  %s", b.problem)
		u.say("  to fix it: %s", b.fix)
	}
	u.say("")
	u.say("Nothing was created and nothing was spent.")
	return &ExitError{Code: graph.ExitFailed}
}

// announceFork prints what a person needs before belay makes the fork and
// spends anything: which step of which run it re-runs, what was asked for,
// where it works, what the history it inherits already cost, what it may
// spend, how to stop it, and every way it differs from the original.
func announceFork(u *ui, plan *runPlan, chosen resumableRun, spec *fork.Spec, treeNote string) {
	u.say("belay is forking run %s in %s", chosen.id, plan.workspace)
	u.say("  what it re-runs:     %s", visitLine(spec))
	u.say("  what was asked for:  %s", orUnrecorded(chosen.goal))
	u.say("  where it works:      a copy of %s; nothing of yours is edited, and nothing is committed", spec.Tree)
	if treeNote != "" {
		u.say("                       %s", treeNote)
	}
	u.say("  history it inherits: %s", inheritedLine(spec))
	u.say("  most it can spend:   %s", u.emph(budgetCeiling(plan.config.Budget)))
	u.say("  when it gets there:  %s", budgetOnExceed(plan.config.Budget))
	u.say("  to stop it:          press Ctrl-C once; belay saves its state and exits")
	for i, d := range spec.Deviations {
		label := "                       "
		if i == 0 {
			label = "  how it differs:      "
		}
		u.say("%s%s", label, d)
	}
	u.say("")
}

// describeFork is `belay fork --dry-run`: everything a fork would do, from
// the decision a real one makes, and then nothing.
func describeFork(u *ui, plan *runPlan, chosen resumableRun, spec *fork.Spec, treeNote string) error {
	u.say("This is a dry run. belay is only describing the work; none of it has happened.")
	u.say("")
	for _, section := range forkSections(u, plan, chosen, spec, treeNote) {
		u.say("%s", section.title)
		for _, line := range section.lines {
			u.say("  %s", line)
		}
		u.say("")
	}

	if len(plan.blockers) > 0 {
		u.say("belay would refuse to fork this run.")
		for _, b := range plan.blockers {
			u.say("")
			u.say("  %s", b.problem)
			u.say("  to fix it: %s", b.fix)
		}
		u.say("")
		u.say("Nothing was created and nothing was spent.")
		return &ExitError{Code: graph.ExitFailed}
	}
	u.say("Nothing was created and nothing was spent. Run the same command without --dry-run to make the fork.")
	return nil
}

// forkSections lists what a fork's dry run describes, in the order it is
// printed: the fork itself, then the run-wide sections every dry run shows,
// built by the same functions. spec is nil when the fork was refused, and
// then only what is known without it is described.
func forkSections(u *ui, plan *runPlan, chosen resumableRun, spec *fork.Spec, treeNote string) []dryRunSection {
	sections := []dryRunSection{{title: "which run it would fork", lines: []string{
		chosen.describe(false),
		"what was asked for: " + orUnrecorded(chosen.goal),
	}}}
	if spec != nil {
		where := []string{
			"a copy of " + spec.Tree,
			"at " + filepath.Join(plan.workspace, ".belay", "forks", "<fork-id>") + ", which is the only thing it edits; it commits nothing",
		}
		if treeNote != "" {
			where = append(where, treeNote)
		}
		sections = append(sections,
			dryRunSection{title: "what it would re-run", lines: []string{
				visitLine(spec) + fmt.Sprintf(", as step %d, from the state the run was in just before it", spec.Snapshot.Step),
				"then it pauses; carrying the fork on is a resume of its own, in its copy",
			}},
			dryRunSection{title: "where it would work", lines: where},
		)
		if len(spec.Deviations) > 0 {
			sections = append(sections, dryRunSection{title: "how it would differ from the original", lines: spec.Deviations})
		}
		sections = append(sections, dryRunSection{title: "what the history it inherits cost", lines: []string{inheritedLine(spec)}})
	}
	sections = append(sections, dryRunSpend(u, plan))
	if models := dryRunModels(plan); len(models.lines) > 0 {
		sections = append(sections, models)
	}
	sections = append(sections, dryRunSettings(plan))
	if guardrails := dryRunPolicy(plan); len(guardrails.lines) > 0 {
		sections = append(sections, guardrails)
	}
	for _, w := range plan.warnings {
		sections = append(sections, dryRunSection{title: "worth knowing", lines: []string{w}})
	}
	return sections
}

// visitLine names the visit a fork re-runs the way `belay timeline` numbers
// it, and says what the step does.
func visitLine(spec *fork.Spec) string {
	line := fmt.Sprintf("%s #%d of %d", spec.Visit.Node, spec.Visit.Ordinal, spec.Visits)
	if meaning, ok := nodeMeaning[spec.Visit.Node]; ok {
		line += " — " + meaning
	}
	return line
}

// inheritedLine says what the history a fork inherits cost, and who paid.
func inheritedLine(spec *fork.Spec) string {
	paid := dollars(spec.InheritedUSD)
	if spec.InheritedEstimated {
		paid = "~" + paid
	}
	return fmt.Sprintf("%s, paid by run %s and not counted again", paid, spec.Source.RunID)
}

// runCommand writes out the belay command that does verb to view's run,
// naming the workspace the run lives in when it is not the one this command
// was pointed at, so the command finds it from wherever it is pasted.
func runCommand(view outcomeView, verb string) string {
	cmd := "belay "
	if view.workspace != "" {
		cmd += "--workspace " + shellQuote(view.workspace) + " "
	}
	return cmd + verb + " " + view.layout.RunID()
}

// pausedAsAskedSteps is what to do about a run that paused because this
// command asked it to stop after a number of nodes, as a fork does after the
// one it re-runs: see what that step did, then carry the run on, if it is
// worth carrying on. Nothing is waiting for a person to read, so the advice
// a gate's pause gets would send them to a plan nobody is waiting on.
func pausedAsAskedSteps(view outcomeView, resume string) []string {
	return []string{
		"it ran the step it was asked to re-run, and paused before " + view.outcome.Node + ".",
		"see what that step did with:",
		"",
		"    " + runCommand(view, "timeline"),
		"",
		"carry the run on from there with:",
		"",
		"    " + resume,
	}
}

// completedInCopySteps is what to do with a run that finished in a copy of
// the workspace, as a fork does once it is carried to its end: its change is
// in the copy, which nothing promotes, so the person reads it there and takes
// what they want by hand. That is the promotion ADR-0004 settled for a
// directory copy, for the same reason: with no branch there is no merge, and
// belay inventing one would be an unreviewable three-way merge.
func completedInCopySteps(view outcomeView) []string {
	return []string{
		"its change is in its copy, at " + shellQuote(view.workspace) + ", and nothing of yours was edited.",
		"see what it did with:",
		"",
		"    " + runCommand(view, "timeline"),
		"",
		"then take what you want from the copy into your own checkout by hand.",
	}
}

// sayForks tells `belay runs` about the forks made from this workspace's
// runs. Each lives in a copy of the workspace, with its run directory inside
// the copy, so it is not among the runs listed here, and a person who forked
// a run and came back for it would otherwise find nothing.
//
// Each fork gets one line: the command that lists it, and which step of which
// run it re-ran, newest first. A copy whose fork cannot be read -- one
// interrupted before its manifest was written -- is listed all the same,
// saying so: a directory belay made is never silently missing from what belay
// says it has. The path is the one found here, not the one the fork's
// manifest recorded, so it is right even after the repository has moved.
//
// It says nothing when there are no forks, and it is not part of
// --json, whose shape is documented and does not change for them.
func sayForks(u *ui, workspace string) {
	root, err := state.ForksDir(workspace)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var lines []string
	for _, e := range slices.Backward(entries) {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		what := "belay cannot read this fork"
		if layout, err := state.NewLayout(dir, e.Name()); err == nil {
			if man, err := state.LoadManifest(layout.ManifestPath()); err == nil && man.Origin != nil {
				what = fmt.Sprintf("%s #%d of run %s", man.Origin.Node, man.Origin.Visit, man.Origin.RunID)
			}
		}
		lines = append(lines, fmt.Sprintf("  belay --workspace %s runs    %s", shellQuote(dir), what))
	}
	if len(lines) == 0 {
		return
	}
	u.say("")
	if len(lines) == 1 {
		u.say("A fork of a run here lives in a copy of this workspace. List it with:")
	} else {
		u.say("%d forks of runs here live in copies of this workspace. List one with:", len(lines))
	}
	for _, line := range lines {
		u.say("%s", line)
	}
}

// shellQuote returns s as a shell reads it back unchanged: as it is when it
// holds nothing a shell would act on, and in single quotes otherwise. A
// workspace on macOS is as likely as not to have a space in its path, and a
// command that breaks on paste is worse than one never written out.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
