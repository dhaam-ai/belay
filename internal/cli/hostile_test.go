package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
)

// hostile is what the security review drove through belay's output: a
// window-title spoof, an OSC 52 clipboard write, and a cursor-up and
// erase-line that hide the line above -- then a C1 control-sequence
// introducer, a NUL and a DEL for good measure.
const hostile = "\x1b]0;PWNED-TITLE\x07\x1b]52;c;cm0gLXJmIH4K\x07\x1b[1A\x1b[2K\u009b2J\x00\x7f"

// hostileShown is hostile as belay must draw it: every control character a
// visible stand-in, so the person sees that something odd is there and the
// terminal acts on none of it.
const hostileShown = "␛]0;PWNED-TITLE␇␛]52;c;cm0gLXJmIH4K␇␛[1A␛[2K<9B>2J␀␡"

// Text belay did not write, each carrying hostile: the goal a person typed
// (or a manifest an agent rewrote), the plan an agent wrote, a note a
// node's error left in the journal, and a file an agent reported changing.
const (
	hostileGoal = "add a helper" + hostile
	hostilePlan = "## Steps\n\n1. Copy " + hostile + " into the clipboard.\n2.\tThen stop.\n"
	hostileNote = "policy: x" + hostile + "y.txt has a line longer than 1048576 bytes"
	hostileFile = "x" + hostile + "y.txt"
)

// assertTerminalSafe fails unless s, which belay printed, could drive no
// terminal: it is UTF-8, and its only control characters are the newlines
// and tabs of belay's own layout. With colour off -- which every test
// stream is, a buffer not being a terminal -- belay itself sends no escape
// sequence at all, so any ESC, C0, C1 or DEL here came from the text.
func assertTerminalSafe(t *testing.T, label, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s is not UTF-8, so a byte of it could be a C1 control:\n%q", label, s)
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			t.Errorf("%s carries the control character %U to the terminal:\n%q", label, r, s)
			return
		}
	}
}

// hostileGoalIn rewrites the goal store's manifest records, the way an
// agent working in place can rewrite any file under .belay.
func hostileGoalIn(t *testing.T, store *state.Store, goal string) {
	t.Helper()
	m, err := store.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	m.Goal = goal
	if err := store.SaveManifest(m); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
}

// TestPlanNoInputShowsAHostilePlanAsStandIns is the review's evidence:
// `belay plan --no-input <run>` printed the plan's bytes and the goal as
// they were, so the terminal set its title, wrote the clipboard and erased
// a line.
func TestPlanNoInputShowsAHostilePlanAsStandIns(t *testing.T) {
	store := pausedAtTheGate(t, hostilePlan)
	hostileGoalIn(t, store, hostileGoal)
	ws, runID := store.Layout().WorkspaceDir(), store.Layout().RunID()

	out, errOut, err := runCLI(t, "plan", "--no-input", "--workspace", ws, runID)
	if err != nil {
		t.Fatalf("belay plan --no-input: %v\n%s%s", err, out, errOut)
	}
	assertTerminalSafe(t, "stdout", out)
	assertTerminalSafe(t, "stderr", errOut)
	for _, want := range []string{
		"what you asked for:  add a helper" + hostileShown,
		"1. Copy " + hostileShown + " into the clipboard.\n2.\tThen stop.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("belay plan does not show %q:\n%s", want, out)
		}
	}
}

// quoted is a named string type, the shape of a status or a node name read
// back from a file an agent can rewrite.
type quoted string

// shown is a fmt.Stringer whose String an agent could have chosen.
type shown struct{ s string }

func (s shown) String() string { return s.s }

// TestSayAndNoteMakeEveryArgumentPrintable: whatever form text from outside
// arrives in -- a string, an error quoting it, a named string type, a
// Stringer -- say and note show its control characters as stand-ins, while
// belay's own emphasis still reaches the terminal as escapes.
func TestSayAndNoteMakeEveryArgumentPrintable(t *testing.T) {
	var out, errOut bytes.Buffer
	u := &ui{out: &out, err: &errOut, color: true}

	u.say("%s|%v|%s|%s|%d|%s", hostile, errors.New(hostile), quoted(hostile), shown{hostile}, 7, u.emph("$5.00"))
	u.note("belay: %v", fmt.Errorf("read %s: %w", hostileFile, errors.New("denied")))

	want := hostileShown + "|" + hostileShown + "|" + hostileShown + "|" + hostileShown + "|7|\x1b[1m$5.00\x1b[0m\n"
	if got := out.String(); got != want {
		t.Errorf("say printed\n%q\nwant\n%q", got, want)
	}
	if got, want := errOut.String(), "belay: read x"+hostileShown+"y.txt: denied\n"; got != want {
		t.Errorf("note printed %q, want %q", got, want)
	}
	if got := u.emph(hostile); strings.Contains(strings.TrimSuffix(strings.TrimPrefix(string(got), "\x1b[1m"), "\x1b[0m"), "\x1b") {
		t.Errorf("emph decorated text it had not made safe: %q", got)
	}
}

// TestTheDryRunCeilingKeepsItsEmphasis: the ceiling is the one line of a dry
// run belay emphasises, and printing every other line as text from outside
// must not turn its escapes into stand-ins.
func TestTheDryRunCeilingKeepsItsEmphasis(t *testing.T) {
	ws := goWorkspace(t)
	var out bytes.Buffer
	u := &ui{out: &out, err: &out, color: true}
	plan, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := describeDryRun(u, plan, hostileGoal, ""); err != nil {
		t.Fatalf("describeDryRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "  \x1b[1m$5.00 or 2,000,000 tokens, whichever comes first\x1b[0m\n") {
		t.Errorf("the ceiling lost its emphasis:\n%q", got)
	}
	if !strings.Contains(got, "  add a helper"+hostileShown+"\n") {
		t.Errorf("the goal is not shown as stand-ins:\n%q", got)
	}
	if strings.Contains(strings.NewReplacer("\x1b[1m", "", "\x1b[0m", "").Replace(got), "\x1b") {
		t.Errorf("the dry run carries an escape that is not belay's emphasis:\n%q", got)
	}
}

// TestTheLastWordIsPrintable: the error a command ends with is printed by
// Execute, after everything else, and is the line a person reads last. A
// node's error quoting a file an agent named -- the review's policy scan
// error -- reached `belay run`'s stderr as it was.
func TestTheLastWordIsPrintable(t *testing.T) {
	for _, err := range []error{
		&ExitError{Code: graph.ExitFailed, Err: errors.New(`graph: node failed: "policy" returned an error: ` + hostileNote)},
		&ExitError{Code: graph.ExitFailed, Message: "belay: " + hostileNote},
		fmt.Errorf("unknown command %q for %q", hostile, "belay"),
	} {
		var out bytes.Buffer
		exitReport(&out, err)
		assertTerminalSafe(t, "the last word", out.String())
		if !strings.Contains(out.String(), hostileShown) && !strings.Contains(out.String(), `\x1b`) {
			t.Errorf("the last word does not show what was there: %q", out.String())
		}
	}
}

// hostileTimeline is a timeline in which every field read from a run's files
// carries hostile: the goal from its manifest, node names, notes and the
// next node from its journal, the files from its blackboard, and the
// candidates' ids and notes from their directories and their runs. In an
// in-place run the agent can write every one of those files.
func hostileTimeline() Timeline {
	tl := BuildTimeline(timelineDemoSelection(), timelineDemoManifest(),
		timelineResult(timelineFixLoopRun(), false))
	tl.Goal = hostileGoal
	tl.Run.Candidate, tl.Run.ParentRunID = "cand"+hostile, "run"+hostile
	for i := range tl.Entries {
		tl.Entries[i].Note = hostileNote
		tl.Entries[i].Next = "next" + hostile
	}
	tl.Entries[0].Node = "plan" + hostile
	tl.Loops = append(tl.Loops, TimelineLoop{Node: "loop" + hostile, Executions: 2})
	tl.Status, tl.Next = "unfinished", "next"+hostile
	tl.Interrupted, tl.InterruptedNode = true, "node"+hostile
	tl.ChangedFiles = []string{hostileFile}
	tl.Candidates = []TimelineCandidate{{
		ID: "cand" + hostile, Index: 1, Outcome: TimelineCandidateFailed, Settled: true,
		SpentUSD: 0.1, Duration: time.Second, Note: hostileNote,
	}}
	tl.Winner = "cand" + hostile
	return tl
}

// TestTimelineShowsWhatItReadAsStandIns is the review's evidence for
// `belay timeline`: the goal and every node's note were printed as they
// were, while the file names and candidates had their control characters
// dropped -- safe, but a name with a byte silently missing can read as a
// different file.
func TestTimelineShowsWhatItReadAsStandIns(t *testing.T) {
	t.Parallel()
	got := timelineRender(t, hostileTimeline(), false)
	assertTerminalSafe(t, "the timeline", got)
	for _, want := range []string{
		"goal    add a helper" + hostileShown + "\n",
		"policy: x" + hostileShown + "y.txt has a line",
		"plan" + hostileShown,
		"  x" + hostileShown + "y.txt\n",
		"cand" + hostileShown,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the timeline does not show %q:\n%s", want, got)
		}
	}

	colored := timelineRender(t, hostileTimeline(), true)
	if rest := regexpSGR.ReplaceAllString(colored, ""); strings.ContainsRune(rest, 0x1b) {
		t.Errorf("with colour on, the timeline carries an escape that is not its own colour:\n%q", colored)
	}
}

// TestTimelineJSONIsExactAndPrintable: a script reading --json gets every
// field exactly as the run's files hold it, and the document is still safe
// to print, because the controls encoding/json leaves raw are escaped.
func TestTimelineJSONIsExactAndPrintable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := RenderTimelineJSON(&buf, hostileTimeline()); err != nil {
		t.Fatalf("RenderTimelineJSON: %v", err)
	}
	assertTerminalSafe(t, "timeline --json", buf.String())

	var doc timelineJSONDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Goal != hostileGoal || doc.Entries[0].Note != hostileNote || doc.ChangedFiles[0] != hostileFile {
		t.Errorf("--json changed what the run recorded: goal %q, note %q, file %q", doc.Goal, doc.Entries[0].Note, doc.ChangedFiles[0])
	}
	if len(doc.Candidates) != 1 || doc.Candidates[0].ID != "cand"+hostile || doc.Candidates[0].Note != hostileNote {
		t.Errorf("--json changed a candidate: %+v", doc.Candidates)
	}
}

// TestRunsAndQueueShowWhatTheyReadAsStandIns: `belay runs` and `belay
// queue list` dropped a goal's control characters, so the table was safe but
// a goal could read as another; their --json documents carried a C1 control
// or a DEL in a goal raw, which encoding/json does not escape.
func TestRunsAndQueueShowWhatTheyReadAsStandIns(t *testing.T) {
	ws := goWorkspace(t)
	p, err := resolve(resolveRequest{workspace: ws, logger: quietLogger()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seedRunDir(t, p, hostileGoal)
	queueAdd(t, ws, hostileGoal)

	for _, args := range [][]string{
		{"runs"}, {"runs", "--json"}, {"queue", "list"}, {"queue", "list", "--json"}, {"queue", "status"},
	} {
		out, errOut, err := runCLI(t, append(args, "--workspace", ws)...)
		if err != nil {
			t.Fatalf("belay %v: %v\n%s%s", args, err, out, errOut)
		}
		label := "belay " + strings.Join(args, " ")
		assertTerminalSafe(t, label, out)
		assertTerminalSafe(t, label+" (stderr)", errOut)
		if !slices.Contains(args, "--json") && !strings.Contains(out, "add a helper␛]0;PWNED-TITLE␇") {
			t.Errorf("%s does not show the goal's escape as a stand-in:\n%s", label, out)
		}
	}
}
