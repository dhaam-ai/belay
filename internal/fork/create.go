package fork

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Fork is a fork Create made: an ordinary run, ready for the dispatcher.
type Fork struct {
	// Layout is the fork's run, inside its copy. Its WorkspaceDir is the
	// copy, which is the tree every node of the fork works in.
	Layout state.Layout
	// Workspace is the copy, as the isolator described it.
	Workspace belay.Workspace
}

// Create makes the fork s describes, as the run runID: a copy of the tree
// made by iso, and inside it a run directory holding the plan and the reports
// the blackboard names, the blackboard, the journal and, last, the manifest.
//
// The manifest goes last because writing it is what makes the run: belay
// resume and belay runs look for manifests and nothing else, so a fork
// interrupted before then is a directory no command takes for a run. A
// failure Create can see takes the copy away again, with the run directory
// inside it, so nothing is left half made; the copy is destroyed under a
// context that cannot be cancelled, because a Ctrl-C is the likeliest reason
// to be unwinding at all.
//
// It refuses with ErrFilesChanged a copy taken while the tree was changing:
// Prepare checked the tree, and a copy is only of that tree if nothing changed
// it in between, so the tree is surveyed again once the copy is made.
//
// now is when the fork was made, which the boundary record and the manifest
// carry.
func (s *Spec) Create(ctx context.Context, runID string, iso belay.Isolator, now time.Time) (f Fork, err error) {
	ws, err := iso.Create(ctx, s.Tree, runID)
	if err != nil {
		return Fork{}, fmt.Errorf("fork: copy %s: %w", s.Tree, err)
	}
	defer func() {
		if err == nil {
			return
		}
		if undo := iso.Destroy(context.WithoutCancel(ctx), ws); undo != nil {
			err = errors.Join(err, fmt.Errorf("fork: and the copy at %s could not be removed: %w", ws.Dir, undo))
		}
	}()

	after, err := s.request.Survey(ctx, s.Tree)
	if err != nil {
		return Fork{}, fmt.Errorf("fork: survey %s: %w", s.Tree, err)
	}
	if after != s.Digest {
		return Fork{}, refuse(ErrFilesChanged, "the files in %s changed while belay was copying them, so the copy "+
			"may hold some of each; try again once nothing is changing them", s.Tree)
	}

	layout, err := state.NewLayout(ws.Dir, runID)
	if err != nil {
		return Fork{}, err
	}
	if err := os.MkdirAll(layout.ArtifactsDir(), 0o750); err != nil {
		return Fork{}, fmt.Errorf("fork: create the run directory %s: %w", layout.RunDir(), err)
	}
	if err := s.writeArtifacts(layout); err != nil {
		return Fork{}, err
	}

	blackboard := s.Snapshot.State
	// The source's conversation went on after the visit; see the package doc.
	blackboard.Code.SessionID = ""
	if err := state.SaveState(layout.StatePath(), blackboard); err != nil {
		return Fork{}, fmt.Errorf("fork: write %s: %w", layout.StatePath(), err)
	}

	history := append(slices.Clone(s.Inherited), journal.Record{
		Seq: s.Visit.StartSeq, Event: journal.EventRunStarted, Time: now.UTC(),
		Note: fmt.Sprintf("fork of run %s, re-running %s: every record before this one is that run's, "+
			"kept without the spending, which that run paid for", s.Source.RunID, s.label()),
	})
	if err := journal.Create(layout.JournalPath(), history); err != nil {
		return Fork{}, fmt.Errorf("fork: %w", err)
	}

	if err := state.SaveManifest(layout.ManifestPath(), s.manifest(now, runID, ws.Dir)); err != nil {
		return Fork{}, fmt.Errorf("fork: write %s: %w", layout.ManifestPath(), err)
	}
	return Fork{Layout: layout, Workspace: ws}, nil
}

// Unrecorded is why a fork's policy gate judges the files its agent reports
// and nothing else, in the words its manifest pins and its verdicts repeat.
//
// A fork's copy is taken part-way through its source run's change. Neither
// ground truth belay has works there: the copy has no .git to ask, and a
// record of the copy would take the source's change as the starting point
// and judge only what the forked node adds to it. A starting point that is
// wrong is worse than none, so a fork says it has none.
const Unrecorded = "a fork works in a copy taken part-way through its source run's change, " +
	"so belay has no record of where that change began"

// manifest is the fork's manifest: a new run under the fork's configuration,
// carrying on from the visit's step, in place, filing no plan, held to the
// guardrails its source was held to, and saying where it came from.
func (s *Spec) manifest(now time.Time, runID, workspace string) state.Manifest {
	m := state.NewManifest(now.UTC(), runID, workspace, s.Source.Goal, s.Config)
	// The node runs as the step it ran as: man.Step counts the steps already
	// taken, and the dispatcher gives the next node man.Step+1.
	m.Step = max(0, s.Snapshot.Step-1)
	m.CurrentNode = s.Visit.Node
	// A copy nobody keeps must not file a plan into itself, whatever
	// plan.archive says: there is nobody to read it there, and in a fork
	// that is resumed to the end it would be the second copy of the source's
	// plan in two places.
	m.ArchivedPlan = state.ArchivedPlan{}
	// The guardrails the history was held to, and the baseline they measure
	// against. A fork cannot take a baseline of its own: its copy is the tree
	// part-way through the source's change, not before it.
	if s.Source.Policy != nil {
		m.SetPolicy(*s.Source.Policy)
	}
	// For the same reason, no ground truth for what changed: see Unrecorded.
	m.SetChanges(state.Changes{From: state.ChangesReported, Why: Unrecorded})
	// The round of the fix loop Prepare gave the fork, if it gave one: the
	// fork's own decision, which its copy's belay.yaml knows nothing of.
	if s.GiveUp > 0 {
		m.SetGiveUpFloor(s.GiveUp)
	}
	m.Origin = &state.Origin{
		RunID: s.Source.RunID, Workspace: s.request.Source.WorkspaceDir(), Tree: s.Tree,
		Node: s.Visit.Node, Visit: s.Visit.Ordinal, StartSeq: s.Visit.StartSeq,
		InheritedUSD: s.InheritedUSD, InheritedEstimated: s.InheritedEstimated,
		Deviations: slices.Clone(s.Deviations),
	}
	return m
}

// writeArtifacts gives the fork's run the plan Prepare found, and a copy of
// each report and diff the blackboard names, so every artifact its
// blackboard points at is in its own run directory.
//
// It copies those and nothing else. The source's artifacts directory also
// holds what the source wrote after the visit, and a fork that held it would
// hold evidence from its own future. A named artifact the source no longer
// has is left out: the source's blackboard pointed at nothing there either,
// and no node reads one back.
func (s *Spec) writeArtifacts(fork state.Layout) error {
	st := s.Snapshot.State
	if s.plan != nil {
		name, _, err := approve.PlanArtifact(fork, st.Plan.Path)
		if err != nil {
			return fmt.Errorf("fork: %w", err)
		}
		if err := state.ReplaceArtifact(fork, name, s.plan); err != nil {
			return fmt.Errorf("fork: write the plan: %w", err)
		}
	}
	for _, recorded := range []string{st.Code.LastDiff, st.Test.ReportPath, st.Review.ReportPath, st.Policy.ReportPath} {
		if recorded == "" {
			continue
		}
		name, abs, err := approve.PlanArtifact(s.request.Source, recorded)
		if err != nil {
			// Not a run artifact by the rule the gate reads plans by, so
			// nothing the fork's run directory could hold.
			continue
		}
		data, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the source's artifacts by PlanArtifact
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("fork: read %s: %w", abs, err)
		}
		if err := state.ReplaceArtifact(fork, name, data); err != nil {
			return fmt.Errorf("fork: write %s: %w", name, err)
		}
	}
	return nil
}
