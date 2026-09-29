// Candidate lanes for `belay timeline`.
//
// This file is the reader's half of best-of-N: it turns a fanout run's N
// candidate runs into lanes a person can scan, and opens one candidate's own
// run as a timeline in its own right. It is deliberately separate from
// timeline.go, which knows only about one run's journal, because everything
// here reads files that belong to *other* runs.
//
// It is READ-ONLY, and that is not a style preference. A candidate's journal
// belongs to the goroutine executing that candidate (ADR-0002: one writer per
// file). journal.Open heals a torn final line by rewriting the file, so a
// reader that used it would rewrite a journal a live dispatcher is appending
// to, and turn the one shape belay guarantees is recoverable into corruption.
// Every read here goes through journal.ReadFile, state.LoadState and
// state.LoadManifest, none of which write anything. See
// TestTimelineNeverRewritesACandidateJournal.
package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/state"
)

// TimelineCandidateOutcome is how one fanout candidate ended, in the
// reader's vocabulary.
//
// It is a separate type from TimelineOutcome because a candidate is a run,
// not a node execution: "refused" and "unfinished" are things that happen to
// a whole run and to nothing else, and a node can be "paused" for a person
// in a way a candidate — which has no approve gate — never is.
type TimelineCandidateOutcome string

const (
	// TimelineCandidateFinished is a candidate run that reached a successful
	// end. It is the only outcome join will select a winner from.
	TimelineCandidateFinished TimelineCandidateOutcome = "finished"
	// TimelineCandidateFailed is a candidate run that ended in failure — its
	// fix loop gave up, or its quality gate refused the change.
	TimelineCandidateFailed TimelineCandidateOutcome = "failed"
	// TimelineCandidateStopped is a candidate run stopped before it could
	// finish, such as one that met its envelope.
	TimelineCandidateStopped TimelineCandidateOutcome = "stopped"
	// TimelineCandidatePaused is a candidate run waiting to be resumed. A
	// resume of the parent takes it up again.
	TimelineCandidatePaused TimelineCandidateOutcome = "paused"
	// TimelineCandidateUnfinished is a candidate run whose journal records no
	// ending at all: claimed and not yet run, or running right now. A
	// read-only reader cannot tell those apart, and does not pretend to.
	TimelineCandidateUnfinished TimelineCandidateOutcome = "unfinished"
	// TimelineCandidateInterrupted is a candidate run that stopped with a
	// node still open, or whose final journal line was cut in half. It is
	// what a crash looks like from outside, and it is expected rather than
	// broken.
	TimelineCandidateInterrupted TimelineCandidateOutcome = "interrupted"
	// TimelineCandidateRefused is a candidate the isolator will never make a
	// workspace for here (state.ProvisionRefused). It never ran, so it has no
	// verdict about anybody's code, and reporting it as "failed" would tell a
	// reader an attempt at their goal failed when none was made.
	TimelineCandidateRefused TimelineCandidateOutcome = "refused"
)

// TimelineCandidate is one candidate's lane: what it did, how it ended, what
// it spent and how long it took.
//
// Where the figures come from depends on whether the fanout has settled, and
// the difference is worth stating because the two sources disagree on
// purpose. Once the fanout node returns, the parent blackboard holds each
// candidate's SpentUSD, DurationMS and Outcome, and those are the figures the
// parent run was billed for and that join compared — so a settled lane reads
// them from there, and Settled says so. While the fanout is still open the
// blackboard is deliberately empty (fanout's pause carries no Patch, so join
// can never see a partial field), so the lane is read from the candidate's
// own run instead: its manifest for identity, envelope and provisioning, and
// its journal for spend and duration. Those are live figures, true "so far"
// and not final.
type TimelineCandidate struct {
	// ID is the candidate's identifier, "cand-01" — bare, not
	// run-qualified, and also its directory name.
	ID string
	// Index is the candidate's 1-based position in the fanout, which fixes
	// the order the lanes are shown in whatever order the candidates
	// happened to finish.
	Index int
	// RunID is the candidate run's own identifier, <parent>-<id>.
	RunID string
	// RunDir is where the candidate's run lives, relative to the parent run
	// directory ("candidate-runs/cand-01"), as state.Candidate.RunDir
	// records it: a run directory can be moved, and a reader holding the
	// parent holds the candidate.
	RunDir string
	// WorkspaceDir is the tree this candidate worked in, or empty for one
	// that never got a workspace. It may no longer exist: promote destroys
	// the losers' workspaces, which is exactly why the candidate's run
	// directory is a sibling of it rather than inside it.
	WorkspaceDir string
	// Outcome is how the candidate ended.
	Outcome TimelineCandidateOutcome
	// Winner reports whether join selected this candidate.
	Winner bool
	// Settled reports whether the figures below came from the parent
	// blackboard (the fanout returned) rather than being read live from the
	// candidate's own run.
	Settled bool
	// TestPassed reports whether the candidate's tests ran and none failed,
	// which is half of what makes a candidate eligible to win — the other
	// half is finishing.
	TestPassed bool
	// IssueCount is how many issues the candidate's quality gate reported.
	IssueCount int
	// Provision is how far the candidate's workspace got, from its own
	// manifest. Empty when no manifest could be read.
	Provision state.Provision
	// ProvisionNote is why, for a candidate whose provisioning was refused.
	ProvisionNote string
	// Envelope is what this candidate run was allowed to spend, from its own
	// manifest — the ceiling it actually ran under, which is not the parent's.
	Envelope state.Envelope
	// SpentUSD is what the candidate spent.
	SpentUSD float64
	// Estimated is true when any part of SpentUSD was a heuristic rather than
	// a reported charge, and is sticky, as everywhere else belay totals
	// money.
	Estimated bool
	// Duration is the sum of the candidate's own node durations, which is
	// what the `fastest` strategy compares. It is not wall clock: the
	// downtime between a crash and the resume that healed it is nobody's
	// fault but the crash's.
	Duration time.Duration
	// Nodes is how many node executions the candidate's journal records. It
	// is 0 for a settled lane, whose figures come from the blackboard, which
	// does not carry a step count.
	Nodes int
	// Interrupted reports a candidate whose journal stops without an ending.
	Interrupted bool
	// Note is a short sentence about how the candidate ended, for a reader
	// who would otherwise have to open its journal. May be empty.
	Note string
}

// Live reports whether this lane's figures were read from the candidate's own
// run while the fanout was still open, and so are true only so far.
func (c TimelineCandidate) Live() bool { return !c.Settled }

// timelineCandidateLanes returns a fanout run's candidate lanes in index
// order, and the id of the winner.
//
// Neither source is required. A run that never fanned out has no blackboard
// candidates and no candidate-runs directory, so it gets no lanes at all and
// its timeline is byte for byte what it was before this file existed — which
// is the guard TestTimelineOfARunWithoutFanoutHasNoLanes keeps.
//
// The lanes are deliberately not gated on manifest.Fanout. `belay timeline`
// exists to work on exactly the damaged run someone is trying to understand,
// and hiding N candidate runs because the parent's manifest was lost would
// fail the one reader who most needs to see them.
func timelineCandidateLanes(parent state.Layout, st state.State, stOK bool) ([]TimelineCandidate, string) {
	if stOK && len(st.Candidates) > 0 {
		return timelineSettledLanes(parent, st), st.Winner
	}
	winner := ""
	if stOK {
		winner = st.Winner
	}
	return timelineLiveLanes(parent), winner
}

// timelineSettledLanes builds the lanes of a fanout that returned.
//
// The blackboard list is already in index order — fanout files each outcome
// at its candidate's position however the goroutines finished — so the
// position in the list *is* the index, which is why state.Candidate carries
// no Index field of its own.
//
// The candidate's own manifest is still consulted, for the envelope and the
// provisioning state, which the blackboard does not carry. The figures are
// not re-read from it: a settled lane must agree with the parent's own spend
// line, and by the time a person reads a finished run, promote may have
// destroyed the workspaces those figures once described.
func timelineSettledLanes(parent state.Layout, st state.State) []TimelineCandidate {
	lanes := make([]TimelineCandidate, 0, len(st.Candidates))
	for i, c := range st.Candidates {
		lane := TimelineCandidate{
			ID:           c.ID,
			Index:        i + 1,
			RunID:        timelineCandidateRunID(parent, c.ID),
			RunDir:       c.RunDir,
			WorkspaceDir: c.Dir,
			Outcome:      timelineSettledOutcome(c.Outcome),
			Winner:       c.ID != "" && c.ID == st.Winner,
			Settled:      true,
			TestPassed:   c.TestPassed,
			IssueCount:   c.IssueCount,
			SpentUSD:     c.SpentUSD,
			Estimated:    c.Estimated,
			Duration:     time.Duration(c.DurationMS) * time.Millisecond,
			Note:         c.Note,
		}
		if layout, err := state.NewCandidateRunLayout(parent, c.ID); err == nil {
			if m, ok := timelineManifestAt(layout); ok && m.Candidate != nil {
				lane.Provision = m.Candidate.Provision
				lane.ProvisionNote = m.Candidate.ProvisionNote
				lane.Envelope = m.Candidate.Envelope
			}
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

// timelineSettledOutcome translates the blackboard's word for how a candidate
// ended into the reader's.
//
// An unrecognised word is carried through rather than guessed at:
// state.Candidate.Outcome is free text by design, so a manifest written by a
// newer belay naming an outcome this one does not know is data to report, not
// a reason to mislabel a candidate.
func timelineSettledOutcome(word string) TimelineCandidateOutcome {
	switch word {
	case "completed":
		return TimelineCandidateFinished
	case "failed":
		return TimelineCandidateFailed
	case "aborted":
		return TimelineCandidateStopped
	case "paused":
		return TimelineCandidatePaused
	case "error":
		// fanout records "error" for a candidate that never ran at all,
		// which today is only ever a durable provisioning refusal.
		return TimelineCandidateRefused
	case "":
		// Every candidate a belay that only prepared workspaces recorded.
		return TimelineCandidateUnfinished
	default:
		return TimelineCandidateOutcome(word)
	}
}

// timelineLiveLanes builds the lanes of a fanout that has not settled, by
// reading each candidate's own run directory.
//
// A directory that is not a candidate run — a torn claim with no manifest, an
// id state.NewCandidateRunLayout refuses — still gets a lane, with whatever
// is legible in it. Leaving it out would hide a candidate the parent is about
// to reclaim, and reclaiming is a thing a reader wants to see coming.
func timelineLiveLanes(parent state.Layout) []TimelineCandidate {
	dirents, err := os.ReadDir(parent.CandidateRunsDir())
	if err != nil {
		// Absent is the ordinary case: no fanout, no directory.
		return nil
	}

	lanes := make([]TimelineCandidate, 0, len(dirents))
	for _, de := range dirents {
		if !de.IsDir() {
			continue
		}
		layout, err := state.NewCandidateRunLayout(parent, de.Name())
		if err != nil {
			// An id the layout refuses is not a candidate belay wrote, so
			// there is nothing under it this reader may safely open.
			continue
		}
		lanes = append(lanes, timelineLiveLane(parent, layout, de.Name()))
	}

	// Index first, so the lanes read in the order the fanout defined them,
	// and the id as the tiebreak for a candidate whose manifest was lost.
	slices.SortFunc(lanes, func(a, b TimelineCandidate) int {
		if a.Index != b.Index {
			return a.Index - b.Index
		}
		return strings.Compare(a.ID, b.ID)
	})
	return lanes
}

// timelineLiveLane reads one candidate run and derives its lane.
//
// It builds the candidate's whole Timeline — the same one `--candidate`
// renders — and takes the figures from that, rather than summing the journal
// again here. One derivation means a lane and the candidate's own timeline can
// never disagree about what it spent.
func timelineLiveLane(parent state.Layout, layout state.Layout, id string) TimelineCandidate {
	sel := TimelineSelection{
		RunID:       timelineCandidateRunID(parent, id),
		Dir:         layout.RunDir(),
		Workspace:   layout.WorkspaceDir(),
		Candidate:   id,
		ParentRunID: parent.RunID(),
	}
	read := timelineOfCandidateRun(sel, layout)
	tl := read.timeline

	lane := TimelineCandidate{
		ID:          id,
		RunID:       sel.RunID,
		RunDir:      timelineCandidateRunDir(id),
		Outcome:     timelineLiveOutcome(tl, read.records),
		SpentUSD:    tl.Totals.USD,
		Estimated:   tl.Totals.Estimated,
		Duration:    tl.Totals.AgentDuration,
		Nodes:       tl.Totals.Nodes,
		Interrupted: tl.Interrupted,
	}
	if manifest := read.manifest; manifest != nil && manifest.Candidate != nil {
		c := manifest.Candidate
		lane.Index = c.Index
		lane.Provision = c.Provision
		lane.ProvisionNote = c.ProvisionNote
		lane.Envelope = c.Envelope
		lane.WorkspaceDir = c.Workspace.Dir
		if manifest.RunID != "" {
			lane.RunID = manifest.RunID
		}
		if c.Provision == state.ProvisionRefused {
			// A refusal outranks whatever the journal says. The candidate
			// never ran, so "unfinished" would promise a reader a resume
			// that will never happen, and the reason is the whole story.
			lane.Outcome = TimelineCandidateRefused
			lane.Note = c.ProvisionNote
		}
	}
	if read.stateOK {
		// Derived exactly as fanout derives it, so a live lane and a settled
		// one mean the same thing by "passed": a test run that ran something
		// and failed none of it.
		lane.TestPassed = read.state.Test.Total > 0 && read.state.Test.Failed == 0
		lane.IssueCount = len(read.state.Review.Issues)
	}
	return lane
}

// timelineLiveOutcome reads how a candidate run ended from its own journal.
//
// The journal is the commit record, and the manifest is not: a kill between
// the run marker and SaveManifest leaves a manifest saying "running" for a run
// that completed, which is why the candidate runner reads the journal too.
// An interruption outranks the resolved state, because a journal with a node
// still open says something a run state cannot.
func timelineLiveOutcome(tl Timeline, records []journal.Record) TimelineCandidateOutcome {
	if tl.Interrupted {
		return TimelineCandidateInterrupted
	}
	res, err := journal.ResolveStart(records)
	if err != nil {
		// ResolveStart refuses to guess at a history that does not pair up,
		// and so does this.
		return TimelineCandidateUnfinished
	}
	switch res.State {
	case journal.RunStateCompleted:
		return TimelineCandidateFinished
	case journal.RunStateFailed:
		return TimelineCandidateFailed
	case journal.RunStateAborted:
		return TimelineCandidateStopped
	case journal.RunStatePaused:
		return TimelineCandidatePaused
	case journal.RunStateRunnable:
		return TimelineCandidateUnfinished
	default:
		return TimelineCandidateUnfinished
	}
}

// timelineCandidateRunID composes a candidate run's own identifier the way
// the candidate runner does: <parent>-<id>. The candidate's Layout knows only
// the bare id, which is its directory name.
func timelineCandidateRunID(parent state.Layout, id string) string {
	return parent.RunID() + "-" + id
}

// timelineCandidateRunDir is a candidate's run directory relative to the
// parent's, spelled the way state.Candidate.RunDir spells it.
func timelineCandidateRunDir(id string) string { return "candidate-runs/" + id }

// timelineCandidateRead is everything one candidate run's own files say,
// gathered in a single read so that a lane and the candidate's own timeline
// are two views of one set of bytes rather than two reads that can disagree.
type timelineCandidateRead struct {
	// timeline is the candidate run's own timeline, exactly as --candidate
	// renders it.
	timeline Timeline
	// manifest is the candidate's manifest, or nil when none was readable.
	manifest *state.Manifest
	// records are the journal records the timeline was built from, which the
	// outcome is resolved against.
	records []journal.Record
	// state is the candidate's blackboard, valid only when stateOK.
	state   state.State
	stateOK bool
}

// timelineOfCandidateRun builds one candidate run's timeline from its own
// files, read-only.
//
// journal.ReadFile, never journal.Open: see this file's package comment. A
// malformed line partway through is real corruption rather than a torn tail,
// and the records before it are still worth showing, which is the same
// judgement LoadTimeline makes about the parent.
func timelineOfCandidateRun(sel TimelineSelection, layout state.Layout) timelineCandidateRead {
	res, err := journal.ReadFile(layout.JournalPath())
	if err != nil && !errors.Is(err, journal.ErrMalformedRecord) {
		// Unreadable for any other reason reads as an empty history: a lane
		// that says "unfinished" is more use to a reader than no lane.
		res = journal.ReadResult{}
	}
	read := timelineCandidateRead{records: res.Records}
	if m, ok := timelineManifestAt(layout); ok {
		read.manifest = &m
	}
	read.state, read.stateOK = timelineStateAt(layout)
	read.timeline = BuildTimeline(sel, read.manifest, res)
	read.timeline.ChangedFiles = timelineChangedFiles(read.state, read.stateOK)
	return read
}

// ErrTimelineCandidateNotFound reports that a run has no such candidate.
var ErrTimelineCandidateNotFound = errors.New("cli: candidate not found")

// TimelineCandidateNotFoundError explains, in a sentence a reader can act on,
// that --candidate named something this run does not have: which id was
// asked for, which run was searched, and which candidates are actually there.
type TimelineCandidateNotFoundError struct {
	// RunID is the parent run that was searched.
	RunID string
	// CandidateID is the id that was asked for.
	CandidateID string
	// Available is every candidate the run does have, in index order.
	Available []string
}

// Error returns the full sentence. It lists the ids that do exist, because a
// candidate id is short, guessable and usually mistyped, and seeing the real
// ones is the fix.
func (e *TimelineCandidateNotFoundError) Error() string {
	if len(e.Available) == 0 {
		return fmt.Sprintf("run %s has no candidate runs — only a run that fans out has candidates", e.RunID)
	}
	return fmt.Sprintf("run %s has no candidate %q — it has %s",
		e.RunID, e.CandidateID, strings.Join(e.Available, ", "))
}

// Unwrap exposes ErrTimelineCandidateNotFound for errors.Is.
func (e *TimelineCandidateNotFoundError) Unwrap() error { return ErrTimelineCandidateNotFound }

// LoadCandidateTimeline builds one fanout candidate's own timeline, the way
// the parent run's is built.
//
// A candidate is a run — its own journal, blackboard, manifest and snapshots
// — so it renders through exactly the same Timeline, RenderTimeline and
// RenderTimelineJSON as any other run. The only difference the reader sees is
// that the selection says which candidate of which parent it is.
//
// The run is resolved through ResolveTimelineRun, so `belay timeline
// --candidate cand-02` with no run id means "the most recent run's cand-02"
// and says which run it chose. A candidate id is never resolved implicitly:
// picking one of N attempts on the reader's behalf would be picking which
// answer they get.
func LoadCandidateTimeline(workspace, runID, candidateID string) (Timeline, error) {
	sel, err := ResolveTimelineRun(workspace, runID)
	if err != nil {
		return Timeline{}, err
	}
	parent, err := state.NewLayout(workspace, sel.RunID)
	if err != nil {
		return Timeline{}, err
	}

	// NewCandidateRunLayout validates the id rather than cleaning it, which
	// is what keeps `--candidate ../../etc` from resolving anywhere.
	layout, err := state.NewCandidateRunLayout(parent, candidateID)
	if err != nil {
		return Timeline{}, err
	}
	if info, err := os.Stat(layout.RunDir()); err != nil || !info.IsDir() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Timeline{}, fmt.Errorf("cli: read candidate run %s: %w", layout.RunDir(), err)
		}
		return Timeline{}, &TimelineCandidateNotFoundError{
			RunID: sel.RunID, CandidateID: candidateID,
			Available: timelineCandidateIDs(parent),
		}
	}

	candSel := TimelineSelection{
		RunID:       timelineCandidateRunID(parent, candidateID),
		Dir:         layout.RunDir(),
		Workspace:   workspace,
		Candidate:   candidateID,
		ParentRunID: sel.RunID,
		Implicit:    sel.Implicit,
		TotalRuns:   sel.TotalRuns,
	}
	read := timelineOfCandidateRun(candSel, layout)
	tl := read.timeline
	if m := read.manifest; m != nil && m.RunID != "" {
		// The candidate's own manifest is its own record of its identity,
		// which beats anything a reader composes for it.
		tl.Run.RunID = m.RunID
	}
	return tl, nil
}

// timelineCandidateIDs lists the candidates a run has, for an error message
// that names the real ids instead of only reporting a miss.
func timelineCandidateIDs(parent state.Layout) []string {
	lanes := timelineLiveLanes(parent)
	if len(lanes) == 0 {
		if st, err := state.LoadState(parent.StatePath()); err == nil {
			ids := make([]string, 0, len(st.Candidates))
			for _, c := range st.Candidates {
				ids = append(ids, c.ID)
			}
			return ids
		}
		return nil
	}
	ids := make([]string, 0, len(lanes))
	for _, l := range lanes {
		ids = append(ids, l.ID)
	}
	return ids
}
