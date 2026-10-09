package cli

import (
	"context"

	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dhaam-ai/belay/internal/exec"
	"github.com/dhaam-ai/belay/internal/filerecord"
	"github.com/dhaam-ai/belay/internal/git"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// resolveChanges settles where a new run will learn what its workspace
// really changed (state.Changes), so the dry run can say it and createRun
// can pin it. It must run after resolveOutput and resolveFanout, which it
// follows.
//
// The answer is the strongest ground truth the workspace the run's agent
// will edit can give:
//
//   - A branch run's agent edits a fresh worktree of the base commit, so its
//     change is whatever git says differs from that commit. The same holds
//     for a branch run that fans out: its candidates are worktrees cut from
//     the same commit.
//   - An in-place run in a git checkout is measured from the commit HEAD is
//     on, and createRun records the files the person had already changed by
//     then, so their uncommitted work is not charged to the run.
//   - Anything else has no git belay can ask: a directory that is not a
//     repository, a copy belay made under .belay (a queued goal's), and the
//     directory copies an in-place run fans out into. Its files are recorded
//     when the workspace is handed to the run, and its change is whatever
//     differs from that record.
//
// It writes nothing: the record is taken by createRun.
func (p *runPlan) resolveChanges(ctx context.Context) {
	p.changes, p.changesRepo = nil, nil
	switch {
	case p.output.Branched():
		p.changes = &state.Changes{From: state.ChangesFromGit, Base: p.output.Base}
		return
	case p.fanout != nil:
		p.changes = &state.Changes{From: state.ChangesFromRecord}
		return
	}
	repo, err := git.Open(ctx, exec.New(p.logger), p.workspace)
	if err == nil && !underBelay(repo.Prefix()) {
		if head, err := repo.Head(ctx); err == nil {
			p.changes = &state.Changes{From: state.ChangesFromGit, Base: head.Commit}
			p.changesRepo = repo
			return
		}
	}
	p.changes = &state.Changes{From: state.ChangesFromRecord}
}

// pinChanges records, in the manifest's one write, where this run's change is
// read from, and takes the record that answer needs: the whole workspace for
// a run with no git, or the files a person had already changed for an
// in-place run in a git checkout. A fanout run takes none of its own; each
// candidate is recorded when its workspace is made.
//
// A record it cannot take refuses the run, as an unreadable dependency
// survey does: a run whose ground truth is missing would be judged against
// nothing, which is the failure this exists to prevent.
func (p *runPlan) pinChanges(ctx context.Context, layout state.Layout, manifest *state.Manifest) error {
	if p.changes == nil {
		return nil
	}
	pin := *p.changes
	var (
		rec filerecord.Record
		err error
	)
	switch {
	case pin.From == state.ChangesFromRecord && p.fanout == nil:
		rec, err = filerecord.Take(ctx, layout.WorkspaceDir(), nil)
		pin.Record = err == nil
	case pin.From == state.ChangesFromGit && !manifest.Output.Branched() && p.changesRepo != nil:
		var dirty []string
		if dirty, err = p.changesRepo.Changes(ctx, layout.WorkspaceDir(), pin.Base); err == nil && len(dirty) > 0 {
			rec, err = filerecord.Take(ctx, layout.WorkspaceDir(), dirty)
			pin.Record = err == nil
		}
	}
	if err == nil && pin.Record {
		err = rec.Save(layout.FileRecordPath())
	}
	if err != nil {
		return fmt.Errorf("belay cannot record what %s holds before the run, so it would have nothing to "+
			"tell the agent's change from what was there: %w", layout.WorkspaceDir(), err)
	}
	manifest.SetChanges(pin)
	return nil
}

// changeSource is graph.Options.Changes: what a run's nodes ask to learn what
// its workspace really changed, built from the pin in the manifest of the
// run being driven, which for a candidate is the candidate's own.
func (p *runPlan) changeSource(man *state.Manifest, layout state.Layout) graph.ChangeSource {
	pin := man.Changes
	switch {
	case pin == nil:
		return unrecorded("this run was created before belay recorded what its workspace held")
	case pin.From == state.ChangesFromGit:
		src := gitChanges{run: exec.New(p.logger), base: pin.Base}
		if pin.Record {
			src.record = layout.FileRecordPath()
		}
		return src
	case pin.From == state.ChangesFromRecord:
		return filerecord.Source{Path: layout.FileRecordPath()}
	case pin.Why != "":
		return unrecorded(pin.Why)
	default:
		return unrecorded("this run keeps no record of what its workspace held")
	}
}

// unrecorded is the ChangeSource of a run with no ground truth: every answer
// is graph.ErrUnrecorded, carrying the reason.
type unrecorded string

// Changes reports that there is nothing to measure against, and why.
func (u unrecorded) Changes(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("%w: %s", graph.ErrUnrecorded, string(u))
}

// gitChanges asks git what a workspace changed since base: internal/git's
// Repo.Changes, less the files the person had already changed when the run
// began, unless the run has changed them since (record).
type gitChanges struct {
	run    *exec.Runner
	base   string
	record string
}

// Changes opens the repository around dir every time it is asked, because a
// candidate's worktree and a resumed run's are not the directory the plan was
// resolved in.
func (g gitChanges) Changes(ctx context.Context, dir string) ([]string, error) {
	repo, err := git.Open(ctx, g.run, dir)
	if err != nil {
		return nil, err
	}
	changed, err := repo.Changes(ctx, dir, g.base)
	if err != nil || g.record == "" {
		return changed, err
	}
	rec, err := filerecord.Load(g.record)
	if err != nil {
		return nil, err
	}
	since, err := rec.Changed(ctx, dir)
	if err != nil {
		return nil, err
	}
	before := rec.Paths()
	out := slices.DeleteFunc(changed, func(p string) bool { return slices.Contains(before, p) })
	out = append(out, since...)
	slices.Sort(out)
	return slices.Compact(out), nil
}

// underBelay reports whether prefix, a workspace's path within its git
// working tree, passes through a .belay directory at any depth, as a copy
// belay made for a queued goal or a fork does. git sees the repository
// around such a copy, and belay's own .belay/.gitignore hides every file in
// it, so git cannot say what it changed.
func underBelay(prefix string) bool {
	return slices.ContainsFunc(strings.Split(prefix, "/"), func(seg string) bool {
		return strings.EqualFold(seg, ".belay")
	})
}

// foldsCase reports whether the file system holding dir treats two names
// that differ only in case as one file, by asking it about dir under another
// spelling. dir must exist and its last element must contain a letter;
// createRun asks about the run's artifacts directory, which does.
func foldsCase(dir string) bool {
	parent, base := filepath.Split(filepath.Clean(dir))
	swapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, base)
	if swapped == base {
		return false
	}
	a, errA := os.Lstat(dir)
	b, errB := os.Lstat(filepath.Join(parent, swapped))
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(a, b)
}

// changesLine says, for the dry run, what the policy gate will judge: every
// file the workspace says changed, and measured how, whatever the agent
// reports; or, for a run with nothing to measure against, that the gate has
// only the agent's report, and why. It is the one place a person can confirm
// before a run what the guardrails will be held against.
func changesLine(plan *runPlan) string {
	pin := plan.changes
	switch {
	case pin == nil:
		return "policy judges the files the agent reports: this run keeps no record of what its workspace held"
	case pin.From == state.ChangesFromGit && plan.output.Branched():
		return "it judges every file git says the run changed since " + shortCommit(pin.Base) +
			", whether or not the agent reports it"
	case pin.From == state.ChangesFromGit:
		return "it judges every file git says changed since " + shortCommit(pin.Base) +
			", apart from what you had already changed when the run starts, whether or not the agent reports it"
	case pin.From == state.ChangesFromRecord && plan.fanout != nil:
		return "each candidate's copy is recorded when it is made, and every file that differs from it is " +
			"judged, whether or not the agent reports it"
	case pin.From == state.ChangesFromRecord:
		return "this workspace is not a git repository belay can ask, so belay records every file in it when " +
			"the run starts and judges every file that differs, whether or not the agent reports it"
	case pin.Why != "":
		return "policy judges the files the agent reports: " + pin.Why
	default:
		return "policy judges the files the agent reports"
	}
}
