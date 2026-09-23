# 15. The interactive plan viewer runs outside the graph

Status: Accepted

## Context

A run stops at the approve gate so a person can read the plan before any
code is written. Until now, reading it meant leaving belay: open
`.belay/runs/<id>/artifacts/plan.md` in an editor, change it, save it, and
type `belay resume <id>`. The owner asked for the obvious thing — "belay
plan viewer in terminal mode so user can add edit plan in same interface and
then proceed — currently go to file and edit".

What makes that more than a rendering job is the approve gate's contract.
The gate does not prompt, does not read stdin and does not wait (ADR-0002,
and `internal/nodes/approve`'s package doc): it returns a paused result, the
dispatcher journals the pause, and the process exits 0. That is what lets a
paused run outlive the SSH session that started it. Any interface that
waited for a person inside a node would make "resumable" mean "resumable as
long as nobody closes their laptop".

Doing it well in a terminal needs raw-mode input, resize handling,
display-width-aware wrapping, and multi-line editing that understands
grapheme clusters. Those are exactly the parts of a hand-written TUI that
rot. belay had three direct dependencies, and each addition is a decision.

Four facts about the libraries, read in their source at the versions pinned
here, shaped the design more than any preference did:

- bubbletea reads `os.Stdin` when it is given no input, and opens
  `/dev/tty` when that is not a terminal (`tea.go`, `Program.Run`). Whether
  belay is at a terminal cannot be left to the library.
- Its colour detection reads `NO_COLOR` with `strconv.ParseBool`
  (colorprofile `env.go`), so `NO_COLOR=yes` leaves colour on, against
  no-color.org; and it runs `tmux info` as a subprocess inside tmux.
- bubbles' textarea runs everything through a sanitizer that turns a tab
  into four spaces and drops other control characters and U+FFFD, and it
  has two keys that shell out to `pbpaste`/`pbcopy`/`xclip`.
- bubbletea has two ways to stop a program, and the one it uses for a
  cancelled context or a SIGINT closes the terminal reader while the
  goroutine reading keys is still using it — a data race the detector
  catches about half the time.

## Decision

belay gains a full-screen plan viewer, `internal/planview`, built on
charm.land/bubbletea/v2 v2.0.9, charm.land/bubbles/v2 v2.2.1 and
charm.land/lipgloss/v2 v2.0.6. `belay plan` opens it, and `belay run`,
`belay resume` and `belay plan --regenerate` open it whenever a dispatch
ends with the gate stopping the run to ask.

**It runs outside the graph, between two dispatches.** The pause is already
durable when the viewer opens, nothing it does runs a node, and it is closed
before the next dispatch begins. Kill belay at any instant and the run is
exactly where the gate left it; the most that is lost is edits nobody saved.
The approve gate still never reads a terminal.

**There is one way a plan enters the graph.** Editing goes through
`internal/plandoc`, which rewrites only the step it was asked to. Saving is
refused by the gate's own rule, `approve.CheckPlan`, written to the gate's
own path, `approve.PlanArtifact`, and installed atomically by
`state.ReplaceArtifact`. Approving is `recordApproval` — what `belay resume`
does, now literally the same function — followed by another dispatch through
`openRun`, so a run working on its own branch continues in its own worktree.
Asking for a new plan is `recordRegeneration`, which records the journalled
approve→plan edge `belay plan --regenerate` records. The viewer itself
writes one file: the plan.

**It opens only where somebody can answer it.** All of: stdin and stdout are
terminals by ioctl (not by file mode — `/dev/null` is a character device);
`TERM` is set and is not `dumb`; `CI` is unset, empty, `0` or `false`; and
`--no-input`, a new persistent flag, was not given. In every other case
belay behaves exactly as it did before: it prints and exits, and the run
waits for `belay resume`.

**It is given its input, its output and its colour profile explicitly**, and
it quits only the way that does not race the library's own reader: a
cancelled context quits the program through `context.AfterFunc`, and a
SIGINT is turned into a quit by a message filter.

**Nothing means anything by colour alone.** The selected step carries a
cursor glyph, unsaved edits are the word "edited", and emphasis — bold, the
same `ui.emph` uses — is kept for the selected step and the spend, the two
things belay emphasises everywhere (ADR-0010). Outside a UTF-8 locale the
viewer draws in ASCII and shows other letters as `?`, because in a Latin-1
terminal the continuation byte of a UTF-8 arrow is a C1 control.

**Plan text cannot drive the terminal.** A plan is written by an agent that
reads the repository and can quote what it found there; every control
character in anything belay did not write becomes a visible stand-in.

## Consequences

### Positive

- Durability is unchanged, and provable: the viewer is a function between
  dispatches, tested by driving its update function with key messages, with
  no terminal anywhere.
- One way in. Whether a person edits the plan in the viewer or in their own
  editor, the same bytes reach the same file, are judged by the same rule,
  and are fingerprinted by the gate.
- A new plan is a step of the run: journalled, charged to the budget,
  visible in `belay timeline`, resumable after a crash.
- The dependency is contained. Only `internal/planview` may import a
  terminal library and only `internal/cli` may import `internal/planview`,
  both enforced by a test that parses every file in the module.

### Negative

- Nineteen modules join the build: 8 direct and 11 indirect, all MIT except
  `github.com/atotto/clipboard`, `golang.org/x/sync` and `golang.org/x/sys`,
  which are BSD-3-Clause; all are compatible with belay's Apache-2.0. One,
  `github.com/charmbracelet/ultraviolet`, has no tagged release and is
  pinned at a pseudo-version.
- `github.com/atotto/clipboard` is linked although both keys that would run
  it are disabled.
- A step holding tabs or control characters cannot be shown in the editor as
  the file holds it. Opening it says so, and a step nobody typed into is
  never rewritten.
- The viewer takes the screen while it is open, so the run's scrollback is
  hidden until it closes. It draws on the alternate screen, and leaving
  prints where the plan is and the commands that come back to it.

### Follow-ups

- Add govulncheck to CI: the module graph is no longer small enough to audit
  by eye.
- Consider an `$EDITOR` hand-off for the parts of a plan that are not steps.
- Consider a lock file on a run directory. A viewer left open widens the
  window in which two belay processes could write to one run;
  `internal/state`'s single-owner assumption is unchanged.

## Alternatives considered

- **Prompt inside the approve node.** No new UI layer at all. Rejected: the
  gate would block on a terminal and a paused run would die with the session
  that started it, which is the one thing the gate exists not to do.
- **Run the dispatcher inside the viewer and show progress there.**
  Rejected: two owners of the terminal, Ctrl-C meaning one thing inside the
  view and another outside it, and a second renderer for the run's story.
- **A hand-rolled raw-mode viewer on `golang.org/x/term`.** One module
  instead of nineteen. Rejected: it means writing grapheme-aware editing,
  display-width measurement, bracketed paste and resize handling, and
  belay's own bug would then be in that code.
- **`$EDITOR` hand-off, the way git asks for a commit message.** No new
  dependency. Rejected as the main interface, because what was asked for is
  structured step editing and a new plan without leaving belay. It remains a
  sensible follow-up.
- **bubbletea v1.** Fully tagged, no pseudo-version. Rejected: v1 is the
  maintenance line, and it brings a comparable number of modules.

## Revisit if

- A security advisory in the charm module graph is not fixed upstream within
  a release cycle.
- Most runs turn out to start without a terminal, so the viewer is rarely
  reached and the modules are carried for nothing.
- A bubbletea release stops honouring an explicit input, output or colour
  profile, which is what keeps belay's headless path headless.

## References

- ADR-0002 (the dispatcher owns persistence), ADR-0010 (the CLI is designed
  against a cognitive model), ADR-0011 (cross-version safety)
- `internal/nodes/approve` (`CheckPlan`, `PlanArtifact`, and the package doc
  on never blocking), `internal/plandoc`, `internal/planview`,
  `internal/cli/interactive.go`, `internal/cli/review.go`
- charm.land/bubbletea/v2 v2.0.9 `tea.go` (`Program.Run`'s `OpenTTY`
  fallback, `Program.shutdown`'s kill path); github.com/charmbracelet/
  colorprofile v0.4.3 `env.go`; charm.land/bubbles/v2 v2.2.1
  `textarea/textarea.go` and `internal/runeutil`
- https://no-color.org/
