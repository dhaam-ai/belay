# Changelog

All notable changes to belay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and belay follows
[semantic versioning](https://semver.org/spec/v2.0.0.html) — with the caveat
that only `pkg/belay` carries a compatibility promise. Everything under
`internal/` may change in any release.

## Unreleased

### Added

- **Best-of-N works: a run with `fanout.enabled` runs its candidates and keeps
  the best.** Until now `fanout.enabled` changed nothing a run did — nothing
  in the graph led to the fanout step, and that step only prepared workspaces.
  A run that fans out now goes plan → approve → fanout → join, and then
  promote when it works on a branch: once the plan is approved, each of
  `fanout.candidates` candidates codes, tests, repairs and is reviewed in a
  workspace of its own, and `join` keeps the one `fanout.select` prefers among
  those whose tests passed, whose own run completed and that broke no `policy`
  rule. A branch run merges the winner onto its branch; an in-place run leaves
  it in its workspace, and the run's last lines name the directory to copy it
  back from. Each candidate is a run of its own, with its own journal, plan,
  blackboard and spend, under `.belay/runs/<run-id>/candidate-runs/`, so a
  crash or a Ctrl-C mid-fanout resumes like any run: finished candidates are
  kept, and only the interrupted ones run again. The new `fanout.concurrency`
  (default 3) says how many candidates run at once; it is read again on every
  resume, so lowering it backs off a rate limit. `budget.max_usd` is divided
  between the candidates before any of them starts, and `belay run --dry-run`
  shows each one's share, the route each candidate walks, how the winner is
  chosen, and how far past the ceiling a fanout can end: up to one step per
  candidate, because a candidate is checked before a step and not during one.
  A candidate that meets its share, `graph.max_steps` or `graph.node_timeout`
  pauses the run like any limit, at exit 3, naming the candidate, why its run
  stopped and the setting to raise; a fault a resume could fix, such as a full
  disk or a git lock, pauses the fanout with the fault in its note; and the
  run's spend in `belay runs` and the closing report includes what its
  candidates have spent, at every pause. `fanout.enabled`, `fanout.candidates`
  and `fanout.select` are recorded when a run starts and a resume keeps them,
  warning if `belay.yaml` has changed since. Several candidates running at
  once are told apart on screen by a `[cand-02]` prefix. `fanout.enabled` now
  requires `budget.max_usd` above zero, since there is otherwise no ceiling to
  divide; it is refused with `review.mode: sonar` or `ai`, whose SonarQube
  projects would be shared by every run's candidates, and refused before
  anything is spent when `fanout.isolator` cannot serve the run's output
  (`dircopy` on a branch, `worktree` in place). `belay resume` refuses a run
  that fans out once `budget.max_usd` is no longer above zero, before it
  starts, and says what to set. Every fanout leaves its N
  candidate-run directories behind, and one that edits in place its N
  workspaces too (a branch run's promote removes its candidates' worktrees);
  belay has no command to remove them yet. ADR-0019 records the decision.
- **`belay fork` re-runs one step of a run without paying again for the steps
  before it.** `belay fork <run-id> fix` makes a new run, with an id of its
  own, in a copy of your repository under `.belay/forks/`, from the state the
  run was in just before that step: it inherits everything before it, runs the
  one step, and pauses before the next, or finishes when the step was the
  run's last. Give a fix loop that gave up another round with a stronger model
  (`--model opus`, for plan, code and fix), or re-run a review after changing
  a setting, and pay for that step alone. A fork of a fix that had used every
  round of `graph.give_up` gets one more, since as it stood it would give up
  again without calling the model; the round is recorded in the fork's
  manifest, so a fork stopped part-way through it and resumed still has it.
  `--visit 2` picks the second time the step
  ran, numbered the way `belay timeline` numbers them; without it belay takes
  the last. The original run is only ever read — not a byte of its journal,
  state or files changes — so both outcomes are there to compare. Carry the
  fork on with `belay --workspace .belay/forks/<fork-id> resume <fork-id>`,
  which belay writes out, and `belay runs` names every fork with the command
  that lists it. Before it spends anything, belay checks what it cannot put
  back: if your files have changed since the step ran it stops and says so,
  because that would be a different experiment, and `--current-files` re-runs
  the step on them as they are now; it names every setting that differs from
  the original, with both values, the extra fix round among them; and the
  agent's conversation never carries over, since the original's went on past
  that step. The fork's spending starts at zero, and what its inherited
  history cost is recorded beside it rather than counted twice. A fork is
  always a single run that edits its copy in place and files no plan, whatever
  `belay.yaml` says; it is held to its source's policy and judged on what its
  agent reports, because its copy cannot say where its source's change began.
  The copy leaves out `.git`, dependency directories such as `node_modules`
  and `vendor`, and any symbolic link that leads outside it, through however
  many links; a Ctrl-C while it is being made removes it. `fanout`, `join` and
  `promote` cannot be forked, nor can a run recorded by a belay that kept no
  snapshots. `--dry-run` describes the fork exactly and creates nothing.
  ADR-0017 records the decision.
- **Line up goals and let belay work through them while you are away.** `belay
  queue add "<goal>"` adds a goal, and `belay queue start` works through the
  queue one goal at a time, each in a copy of the workspace made when belay
  reaches it, under `.belay/queue/work/`, so your checkout is never edited.
  The copy is made like a fork's, without `.git` or dependency directories,
  and every goal runs in place in it. Before it spends anything, a start says
  how many goals it will run, the most each can spend (`budget.max_usd`) and
  the product; it refuses `budget.on_exceed: warn`, since nobody is watching a
  queue to stop an overrun. A goal whose plan stops for review is parked, and
  the queue moves on to the next: `belay queue approve <goal>` releases it,
  and the next start carries it on. Only a goal you add with `--no-approval`
  skips the gate: the queue does not read `graph.approval`, so turning it off
  in a `belay.yaml`, or in your own `config.yaml`, never takes a queued goal
  past it. Ctrl-C pauses the goal in progress, and a crash, a kill or a reboot
  loses nothing: `belay queue start` again picks up the goal it had in hand,
  in the same run, and then the rest. The queue records each goal's claim
  before it makes anything for it, so no crash can run a goal twice. `belay
  queue` on its own is the morning report: what the last start did, what it
  spent, and the exact command for each goal that needs you. The reports read
  each goal's run, so a goal you carried on by hand shows as its run now
  stands, and a goal whose run fanned out names the winning candidate's
  workspace, where its change is. `belay queue list --json` prints every goal
  with its state, run, copy and spend, for scripts, and `belay queue rm` takes
  back a goal nothing has started. Taking a finished goal's change from its
  copy is up to you. `belay runs` still prints the JSON it always did, and
  under its table names the runs that live in the queue's copies. ADR-0017
  records the decision.
- **Guardrails a run's change must obey, whatever its tests say.** The new
  `policy` section in `belay.yaml` declares paths a change may not touch
  (`forbidden_paths`), ceilings on its size (`max_changed_files`,
  `max_changed_lines`), a ban on declaring a dependency the workspace did not
  already declare (`no_new_dependencies`), a rule that a change of one kind
  must bring a change of another with it (`required_tests`, typically a test
  beside the code), and text a changed file may not contain
  (`forbidden_content`). A guarded run gains a `policy` step between `write`
  and `test`, so the change is judged before anything else is spent on it and
  again on every pass of the fix loop. The gate judges what the workspace says
  changed, not what the agent says it changed: in a git repository, every file
  git says differs from the run's starting commit, committed, staged, edited,
  deleted or new, in its true name; in place in a git checkout, less what you
  had already changed before the run; with no git (including a queued goal's
  copy and an in-place run's candidates), every file that differs from a
  record of the workspace taken when the run starts. Files the agent changed
  and did not report are judged like the rest and named in the gate's verdict,
  `belay timeline` and the commit, and on a disk that folds case, as macOS's
  does, rules match without regard to case. A fork, whose copy cannot say
  where its source's change began, is judged on its agent's report and says
  so. A forbidden path or an exceeded ceiling ends the run, because a change
  that touched what it may not touch is not something to ask an agent to
  repair; the other rules go to the fix loop, which tells the agent what it
  broke and asks for a repair, bounded by `graph.give_up`. Violations reach
  you as the same report a linter's findings do, archived per step, and `belay
  run --dry-run` prints every rule, says which ones would end a run, and says
  what the gate will judge. Unlike every other setting, policy combines across
  configuration files to the most restrictive result — lists are joined, each
  ceiling is the smallest any file sets, and `no_new_dependencies` holds if
  any file sets it — so a repository's `belay.yaml`, a file the agent can
  edit, cannot drop the guardrails in your own `~/.config/belay/config.yaml`.
  A rule belay cannot honour — an unusable path glob, a pattern that is not a
  regular expression — is refused when the configuration loads rather than
  skipped at the gate: a guardrail that silently does nothing is worse than
  none, because somebody is relying on it. The rules, and a survey of what
  your workspace declared, are recorded when the run starts, so `belay resume`
  holds a run to the rules it began with and warns if `belay.yaml` has changed
  since — a policy added mid-run has no record of the tree from before the
  agent ran, and could only fail the run for something it did not do.
  `max_changed_lines` counts the whole of every file the change touches rather
  than a diff; a one-line edit to a 900-line file counts 900. The gate cannot
  see what the agent writes outside the workspace, a file `.gitignore`
  excludes in a git checkout, or a name that differs from a pattern only in
  its Unicode normalisation; `docs/config.md` says what it judges in each kind
  of run. Best-of-N is not a way around a rule: `join` will not promote a
  candidate whose change broke one. Repositories that declare no policy run
  exactly as they did. ADR-0014 records the decision, and `docs/config.md`
  documents every setting.
- **`belay timeline` shows what a best-of-N run's candidates did.** A run
  that fans out now gets a lane per candidate after its totals: how each one
  ended, whether its tests passed, how many issues its quality gate found,
  what it spent, how long it took, and which one won. Once the fanout
  finishes the figures are the ones the run was billed for; while it is still
  going they are each candidate's own running total, read from that
  candidate's journal, and the timeline says so — and names the envelope each
  candidate is capped by, because the run's own ceiling is not what bounds a
  running candidate. `belay timeline <run-id> --candidate cand-02` opens one
  candidate's run as a timeline in its own right, with its own steps, spend
  and changed files, and a mistyped id is answered with the ids that do
  exist. Every candidate file is read without being written to: a candidate's
  journal belongs to the run that is writing it, and a reader that healed a
  torn final line would corrupt the one failure belay can always recover
  from. `--json` gains `candidates`, `winner`, `candidate` and
  `parent_run_id`; nothing already in that document changed, so its schema
  version is unchanged, and a run that does not fan out prints exactly what
  it printed before.
- **Each step can call a model of its own.** `agent.models` routes a node to a
  model, so the strong one writes the plan and repairs a failure while the
  cheap one does the mechanical middle, and the same `budget.max_usd` buys a
  longer run. A node with no entry is sent `agent.model`, so a configuration
  without it costs exactly what it did. Only the nodes that call a model may
  be keyed — `plan`, `code` and `fix` — and any other name is refused when the
  configuration loads, by name and with the names it could have been: a typo
  that quietly sent the whole run to one model is the failure that would make
  routing not worth having. `belay run --dry-run` prints a row per node with
  the model it would use and that model’s list rate, and states no total,
  because belay cannot know how many tokens a goal will take and a wrong
  dollar projection is worse than none. Every model a run would use is checked
  against the backend before the run starts, not just `agent.model`, so one
  the backend refuses stops the run before it spends rather than in the middle
  of it. The Claude Code backend refuses none: a dated identifier such as
  `claude-haiku-4-5-20251001` and the aliases `sonnet[1m]` and `opusplan` are
  priced as the model they name, and a model its price table does not list,
  such as `claude-sonnet-4-5` or one released after this belay, is priced at
  the most expensive rate it knows, marked as an estimate, with a warning
  naming it, so the budget ceiling still holds and errs towards stopping
  early; `--dry-run` marks such a rate "(assumed)", and the warning is shown
  without `--verbose`. The table is only used when the CLI does not report a
  call's cost itself. Spend is recorded against the model that actually ran each step, so
  `belay timeline` shows the cheap step cheap.
- **A run works on its own branch and never edits your checkout.** In a git
  repository, `belay run` now creates `belay/<run-id>` at the commit you are
  on, checks it out in its own worktree under `.belay/runs/<run-id>/`, and
  works there. It commits once, after the quality gate passes, under your own
  git identity with a `Belay-Run: <run-id>` trailer, and it commits exactly
  what the run's steps recorded and judged: if the worktree holds anything
  else when the run reaches that commit — an edit made while the run was
  paused, a file the test command wrote, a change above the directory a run
  was started in — the run pauses and names the files, and a guarded run's
  change is judged against its policy once more first. The commit step,
  `promote`, calls no model, so the budget ceiling is not checked before it,
  nor before `join`: a run that reached `budget.max_usd` in its last coding
  step still delivers what it paid for. Your checkout keeps its branch, its
  commit and its uncommitted work, and the run's result is a branch you can
  read with `git log -p`, take with `git switch`, or delete. Outside a git
  repository, or with `output.mode: "in_place"`, belay edits the workspace
  directly, as it always did. The new `output.mode` setting chooses, and
  `auto` — the default — picks a branch whenever the workspace can take one (a
  commit to branch from, git 2.38 or newer, no submodules) and says why when
  it cannot; a workspace inside `.belay`, such as a queued goal's or a fork's
  copy, always edits in place. `belay resume` finishes a run the way it
  started, whatever `belay.yaml` says by then, and refuses a run whose
  worktree has been deleted with the git command that makes it again. ADR-0012
  records the decision.
- **`belay run --pr` opens a draft pull request.** When the run finishes,
  belay pushes its branch and opens a draft pull request through `gh`,
  GitHub's own command line. It checks before the run starts that it could:
  in-place output has no branch to propose, `gh` may not be installed, the
  repository may have no remote, and the end of a run is the worst moment to
  find out. A pull request that already exists for the branch is reported
  rather than opened again, and one that cannot be opened never fails a run
  that finished — the closing report names the two commands to finish by hand.
- **A finished run puts its plan beside the code it produced.** When a run
  completes, belay moves the plan it followed out of
  `.belay/runs/<run-id>/artifacts/` and into the repository, as
  `docs/plans/<run-id>-<slug>.md`. A run on a branch puts it in the run's one
  commit, written just before that commit, so it is part of what the branch
  and the pull request deliver; a run that edits the workspace in place leaves
  it as a new file beside the change, for you to commit or delete — belay
  stages nothing. It is a move, and a careful one: the plan is written whole
  and fsynced, then the manifest records where it went, then the run's copy
  goes, so a crash anywhere leaves the plan in at least one place and running
  the move again finishes it instead of filing a second copy. belay never
  replaces a file already there, never writes outside the workspace, and never
  files the plan of a run that failed or of a plan edited after the run last
  read it; when it cannot move a plan it says why, and `belay plan --archive
  <run-id>` finishes the move once the way is clear. `plan.archive` and
  `plan.archive_dir` choose whether and where, and belay says where the plan
  will go in `--dry-run` and in the run's opening lines. ADR-0016 records the
  decision.
- **belay keeps its own scratch out of your commits.** The first run in a
  repository writes `.belay/.gitignore` holding `*`, so run state is never
  offered for a commit by accident. One you wrote yourself is never replaced.
  The recording `agent.mode: record` saves lives under `.belay` by default and
  is ignored with the rest; record it elsewhere with `--cassette` if you mean
  to commit it.
- **Fanout candidates can be git worktrees.** `fanout.isolator` accepts
  `worktree`, which puts each candidate on a branch of its own so a winner
  can be merged rather than copied back by hand, and `auto`, which follows
  the output mode. `dircopy` stays for targets that are not git repositories.
- **`belay plan` reads the plan a run is waiting on, and can ask for a new
  one.** `belay plan` prints the plan of the run belay picked, where the file
  is, and what to do with it. `belay plan --regenerate "fewer steps, and leave
  the parser alone"` is the other half: belay keeps a copy of the plan it is
  about to replace, records what you asked for, and continues the run so the
  plan step writes a new plan with your words in its prompt — then stops at
  the plan again so you can read it. Asking is a step of the run like any
  other: it is journaled, it counts against the budget, `belay timeline` shows
  it, and `belay resume` picks it up if it is interrupted. Neither form needs
  a terminal. Name a run that is over and `belay plan <run-id>` shows its plan
  from wherever it is: the copy in your repository, the run's commit for a
  branch you have not taken yet, or the run directory when the plan never
  moved. `belay resume` still means "the plan as it stands is approved", so
  resuming after asking for a new plan withdraws the request instead of
  spending on one, and says so.
- **A plan can be read, edited and approved in belay, without leaving it.** At
  a terminal, `belay plan` now opens the plan in a full-screen viewer: scroll
  it, edit a step in place, add, delete and reorder steps, undo, ask the agent
  for a different plan in your own words, and approve it and carry on — all in
  one place, with what the run has spent shown before you commit it to
  spending more. `belay run` and `belay resume` open it too, whenever a run
  stops at its plan and there is a terminal to open it on. Press q to leave:
  the run stays exactly where it was waiting, and belay says how to come back.

  Nothing about belay without a terminal changes. The viewer opens only when
  belay is reading from and writing to a real terminal, `TERM` is set and is
  not "dumb", `CI` is unset (or `0` or `false`), and `--no-input` — a new flag
  for saying so outright — was not given. Anywhere else, and under
  `--dry-run`, every command prints exactly what it printed before and exits,
  and the run waits for `belay resume` as it always has.

  What the viewer does is what you could already do by hand: it edits the same
  plan file, which it saves only when the approve gate would accept it, and
  approving is `belay resume` and asking for a new plan is `belay plan
  --regenerate`. It never runs a step of the run itself — it opens after the
  run is durably waiting and closes before belay dispatches again — so
  stopping belay while it is open loses nothing but edits you had not saved.
  ADR-0015 records the decision, including the nineteen modules it adds, all
  MIT or BSD-3-Clause.
- **`belay timeline` lists the files a run changed.** After the totals, a
  "Files changed:" section lists every file the run changed, one per line and
  sorted, including the files the fix loop repaired. It is left out when the
  run has changed nothing. `belay timeline --json` carries the same list as
  `changed_files`. The README's example output showed this section before
  belay printed it, in a table layout belay never used; it is now rendered by
  belay's own timeline code from the journal of a run with a fix loop, and a
  test keeps it that way.

### Changed

- **`fanout.isolator` defaults to `auto`, and is a closed set.** It was free
  text checked only for emptiness, so `worktrees` or `dircopy ` loaded fine
  and failed later, when a run had already been created — while
  docs/config.md claimed `dircopy` was the only accepted value. It now
  accepts `auto`, `worktree` or `dircopy` and rejects anything else by name.
  The default changes from `dircopy` to `auto`, which keeps it in step with
  `output.mode`.
- **belay v0.1.x can no longer resume a run this version has saved.** belay
  ignores fields it does not know in a run's state.json and manifest.json, and
  saves the run without them. Once runs carry settings that change what they
  do, an older belay resuming one would drop those settings without a word.
  Both files now carry a `requires` list naming what a run depends on, and a
  belay that lacks something on the list refuses the run by name and says to
  upgrade. A run on a branch, a run with a policy, a run that fans out (and
  each of its candidate runs), and a plan waiting to be regenerated each name
  what they need; a run that edits in place with none of these requires
  nothing. v0.1.x never reads the list, so both files also move to schema
  version 2, which v0.1.x refuses: its `belay runs` shows such a run as
  "unsupported manifest version 2", and its `belay resume` does not offer it.
  Runs started by v0.1.x still resume, and are saved at version 2 from then
  on. With two belay installs, resume runs with the newer one. ADR-0011
  records the decision.
- **A run belay stops at one of its own limits is paused, not ended.**
  Reaching `budget.max_usd` or `budget.max_tokens`, running out of
  `graph.max_steps`, or overrunning `graph.node_timeout` used to end the run
  for good: the only way on was a new run from nothing, which would spend the
  whole budget again from zero. belay now pauses the run and says which
  setting to raise. Raise it in belay.yaml and `belay resume <run-id>` carries
  on with everything the run has already spent still counted; leave it and the
  run stops at the same limit again, having done nothing and spent nothing.
  These limits are read from belay.yaml every time a run is resumed, unlike
  the shape of the graph, which is fixed when a run starts. A step that
  overran `graph.node_timeout` runs again from its start. The exit code is
  unchanged: an invocation belay stopped this way still exits 3, so scripts
  can tell it from a run that paused for you to read the plan. `belay runs`
  lists such a run as waiting and `belay resume` accepts it. A step that stops
  for the same reason, because it can see the ceiling will not cover what it
  is about to buy, as the fanout step can, pauses the run the same way.
- **ruff judges only the lines a run changed, the way golangci-lint already
  does.** The lint gate on a Python project counted every ruff finding in the
  repository, so a repository with existing debt failed every run whatever
  the run did, and the fix step was sent to repair code the run never
  touched. belay now asks git which lines differ from HEAD, adds every line of
  a file the run created, and counts only the findings on them. A syntax
  error, or a finding with no line, counts anywhere in a file the run
  touched, because a parser reports where it gave up rather than where the
  edit was; one in a file the run left alone does not, because Python
  repositories commit files that do not parse on purpose, as test data.
  Where git cannot say what changed — no repository, no commit yet, an
  ignored directory — every finding counts, belay logs why, and the review
  summary says so. A renamed file counts in full. ESLint still counts every
  finding.

### Fixed

- **belay says what it warns about on real runs, not only under `--dry-run`.**
  Uncommitted work a branch run leaves out, a resumed run's settings
  disagreeing with `belay.yaml`, a credential belay cannot see: each was
  collected for every run and printed only by `--dry-run`, so the run itself
  said none of it. `belay run`, `belay resume`, `belay fork` and each queued
  goal now print them on stderr, after the announcement and before anything
  is spent.
- **A repair's changes are recorded and checked like the first change.** belay
  keeps a list of the files a run changed, taken from what the agent reported
  after writing the code. When a test or the quality gate failed and the agent
  repaired it, the files the repair touched were never added, so the list
  belay passes to a reviewer, and shows the agent on its next repair, left
  them out. And the fix step routed straight back to the tests, so a repair
  was the one change in a run that nothing verified: no claimed path was
  proven to be a regular file inside the workspace, no diff of the repair was
  recorded, and the run's `last_diff` still pointed at the original change
  however many repairs had happened since. A repair now goes back through the
  `write` step, which adds the files it changed to the list, each once, checks
  every one against the disk, records a diff and moves `last_diff` on. A
  repair that reports a path outside the repository fails the run, as the
  first change already did.
- **A step belay re-runs after a crash no longer counts its crashed attempt
  as well as its own.** belay saves a step's changes and only then records
  that the step finished, so a process killed in between left the changes
  saved and the step unfinished, and resuming ran the step again — reading
  the changes its own crashed attempt had already saved. For the fix step
  that meant counting one repair twice: "fix attempt 2 of 3" when only one
  repair had been made, and a run could give up a repair early. belay now
  keeps a copy of what each step is given under `snapshots/` in the run's
  directory, taken before the step runs, and a step run again after a crash
  or a Ctrl-C is given that copy. It costs one small file per step. A run
  started before this version keeps the old behaviour for the steps it had
  already run.
- **Emptying the plan while a run waits for you no longer ends the run.**
  The approve gate read `artifacts/plan.md` again as it ran, and a file
  that had been emptied — or saved in an encoding that is not UTF-8 — ended
  the run with an error. `belay resume` refuses a run that has ended, so an
  editing slip cost the whole run and everything it had already spent. The
  gate now pauses instead and says which file is wrong and what is wrong
  with it; put a plan back in the file and `belay resume <run-id>` carries
  on. A plan that is missing altogether is still an error: there is nothing
  there for anyone to fix.
- **The README describes what belay really does and prints.** Its dry-run
  example showed a ceiling of 1,000,000 tokens, 5 agent turns a step and a
  closing line belay never printed, and left out what reaching the ceiling
  does and when belay gives up. It is now exactly what `belay run --dry-run`
  prints for the quickstart's own belay.yaml, and a test keeps it that way.
  The README also sent you to `.belay/runs/<run-id>/plan.md`, when the plan
  is at `artifacts/plan.md`; said manifest.json holds the run's state, which
  is in state.json; listed agent modes belay does not have (streaming,
  batch) instead of live, record and replay; said a failing quality gate
  pauses the run, when it sends the change to the fix step; said nodes
  never touch the filesystem; and put writing the journal before saving a
  step's changes, the reverse of the order belay depends on.
- **The docs no longer claim what belay does not do.** The README called belay
  "CLI-agnostic" and said the same workflow "runs against any backend without
  code changes", when Claude Code is the only backend and the seam has never
  been tried with another; docs/adapters.md listed a `CodeFormatter` interface
  that does not exist and cited an `internal/review` package that does not
  either. The README described recording test cassettes with a `make
  test-record` target, and docs/adapters.md with a `cassette_record` build
  tag; neither exists, and the adapters' fixtures are hand-written. The README
  also called the module path a placeholder (it is the real one) and suggested
  `go get ...@latest` to install a command, named macOS 11 as Monterey (it is
  Big Sur, and unsupported because Go 1.26 binaries need macOS 12), and said
  `belay runs` lists all runs (the latest 20, unless `--limit` says
  otherwise). Each now says what the code does.
- **belay no longer says its write step saves the agent's changes.** The
  closing report described the write step as "saving the agent's changes
  into your repository", and `belay run --help` said belay "saves the
  files". The agent edits the files itself, in place; the write step checks
  the files it reported changing and records them, and never writes to your
  repository. Both now say that.
- **A dry run with `graph.approval: false` no longer shows the approve gate on
  the route.** The route belay prints is the walk a run takes, and with
  approval off the plan node goes straight to code — which the line under the
  route already said, while the route above it still listed `approve`. The gate
  is still part of the graph belay can resolve, so a run started with it and
  resumed after the flag was turned off still re-runs the step its journal
  names.
- **`belay resume <run-id>` says a damaged run is damaged, instead of saying
  it does not exist.** A run whose manifest.json belay cannot read was skipped
  when choosing a run, and naming it answered "belay has no run called
  <run-id>" — for an id a person had usually just copied from `belay runs`,
  which lists the same run and says what is wrong with it. Naming such a run
  now says belay cannot continue it, why in the same words `belay runs` uses,
  and where the run directory is. belay still never picks one when you do not
  name a run: it passes over it and chooses from the others. `belay resume
  --help` says both.
- **`belay timeline` no longer reports the approval gate as a loop.** The gate
  pausing for you and running again when you resumed the run is two journal
  records for one visit, and the timeline counted them as two: every run with
  `graph.approval` on said "approve ran 2 times", numbered the two rows
  "approve #1/2" and "#2/2", and opened the loop-cost span at the gate — so the
  figure for what the test-and-fix loop cost included the plan, the code and
  the write that came before it. The same held for a step that Ctrl-C or a
  crash interrupted and belay ran again. Both now read as one visit, and the
  loop cost covers the loop. The README's example output is regenerated from
  what belay prints.

  A script reading `belay timeline --json` will see different numbers for
  such runs. `loops[].executions` now counts visits to a node; it counted
  journal records, which over-counted every run with the gate on, so
  `approve` no longer appears in `loops`. Each entry's `iteration`
  and `iterations` count visits too, and the `loop_*` totals start at the
  first node that really looped. `schema` stays 1: these fields always meant
  how many times the run came back to a node, and the old values were wrong.
- **A repair that quotes the failure it fixed no longer poisons or ends the
  run.** The fix node read its reply with the code node's parser, which, when
  no changed-files block is present, salvages paths out of any unified diff in
  the text. The fix prompt hands the agent its test failures and asks what was
  broken, so a reply quoting one is ordinary — and a test failure is routinely
  a diff. A quoted testify comparison recorded a file called "Actual" as
  changed, which then reached the reviewer, the next repair's prompt,
  state.json and `belay timeline`; a quoted golden-file `diff -u`, whose paths
  are absolute, was refused as an unusable report, which failed the node and
  ended the run for good after the repair had been made and paid for. A
  repair's reply is now read for the changed-files block alone. The code node
  is unchanged: its own prompt quotes no diff.
- **`review.ai.mcp.url` is now required for `review.ai.mcp.mode: cloud`
  when `review.mode` is `ai`.** `cloud`, the default mode, has no built-in
  address, so the AI reviewer needs the URL as much as `server` does, but
  the loader required it only for `server`. No run was at risk: `belay run`
  and `belay resume` refuse `review.mode: ai` before they start, because
  that reviewer is not wired in yet. What changes is where such a
  configuration stops: it now fails when belay loads it, with an error
  naming `review.ai.mcp.url`, instead of loading and meeting that refusal.
  The refusal itself now says what the mode is, as docs/config.md does; it
  used to call the reviewer "agent-driven" and "not built yet". Under any
  other `review.mode` the URL may still be left empty, so the default
  configuration is unaffected.
- **`--config` help no longer says the file replaces belay.yaml.** It said
  belay reads the file "instead of" the belay.yaml it would find, but belay
  reads your user config and the workspace's belay.yaml as well, and merges
  the `--config` file on top of them, setting by setting. A setting you
  left out of that file still came from the others. The help, and the
  README's configuration section, now say so.
- **A run a newer belay wrote now says it needs a newer belay.** A run
  whose manifest.json requires something this belay does not have, or is
  at a schema version above the one it reads, is not damaged, but
  `belay runs` listed it as "manifest.json is damaged" or "unsupported
  manifest version", and `belay resume` acted as if it were not there:
  named, it said there was no such run, and left to choose, it continued
  an older run instead. `belay runs` now lists it as "needs a newer belay",
  with what it requires. `belay resume` refuses it with an error that says
  what it requires and to upgrade belay, and while such a run is in the
  workspace, `belay resume` with no run id will not choose a run for you;
  name the run instead. A version older than any belay reads keeps its
  "unsupported" wording.
- **Ctrl-C now leaves a run that `belay resume` continues.** `belay run
  --help` promised this, but Ctrl-C, or a SIGTERM, ended the run as
  aborted: belay printed `belay resume <id>`, and `belay resume` then
  refused the run as "already stopped for good". belay now pauses the run
  at the step it was in. `belay resume` runs that step again from its
  start, and not the steps that had finished. If Ctrl-C lands while belay
  is at the approval gate, resuming asks for approval again rather than
  approving the plan for you. An interrupted `belay run` still exits with
  code 3. A second Ctrl-C still quits at once, which leaves the run as a
  crash does, and `belay resume` continues that too.
- **belay no longer tells you to resume a run it cannot resume.** A run that
  ended as aborted was offered `belay resume <id>` in the closing report, and
  `belay resume` refuses such a run. The report now says the run has ended and
  points at `belay timeline`. (A run stopped at `graph.max_steps`,
  `graph.node_timeout` or the budget ceiling is now paused, and the report
  names the setting to raise before resuming; see *Changed*.)
  docs/troubleshooting.md and docs/architecture.md also suggested `belay
  resume` for runs that had completed or failed, and no longer do.
- **A crash while belay recovers from a crash no longer ends the run.**
  After a crash belay closes the step that was cut off and runs it again.
  If belay died again between the two, the next attempt took the run for
  aborted and refused it. It now runs the step again, and numbers it as
  the next attempt instead of reusing the number of the one it replaces.
- **A `belay run` that fails while creating its run no longer leaves a run
  that `belay resume` picks up.** belay wrote the run's manifest.json before
  its state.json. If writing state.json failed, or belay was killed between
  the two, the manifest said the run was running: `belay resume` chose it
  and then failed to load it, and `belay runs` listed it as resumable. belay
  now writes state.json first and the manifest last, so a run exists only
  once both files do. A run whose creation was cut short shows in
  `belay runs` as unreadable, with no manifest.json, and `belay resume`
  never offers it.
- **The configuration reference now states the defaults and values belay
  actually uses.** docs/config.md had drifted on nine defaults, among them
  `agent.max_turns` (30, not 5), `graph.max_steps` (60, not 20),
  `graph.node_timeout` (15m, not 10m) and `budget.max_usd` (5, not 10). It
  listed values the loader rejects for `agent.mode`, `test.runner`,
  `review.fail_on` and `review.ai.mcp.mode`, and documented
  `review.sonar.quality_gate_wait` and `quality_gate_timeout` as durations
  when they are a boolean and a number of seconds. It also said unknown keys
  are only warnings, when they are errors, and that environment variables and
  flags override settings: none does, apart from the few settings `belay fork`
  and `belay queue` decide for themselves, and a `--config` file is how to
  change one for a run. Tests now check every documented setting's type,
  default and valid values against the code, and fail when a setting has no
  entry.
- **A step that fails, times out or is stopped is charged for what it spent.**
  When the agent reported spending money and the step then returned an error,
  ran past `graph.node_timeout`, or was stopped with Ctrl-C, belay recorded
  the step as free: the money was left out of the run's total and out of the
  journal. A run could then go past `budget.max_usd`, and a run stopped with
  Ctrl-C and resumed could spend its whole budget a second time. Whatever the
  agent reported spending is now added to the run's total and written to the
  journal, however the step ended. The agent CLI prints its bill only when it
  finishes, so a step belay stopped often reports nothing; such a step is now
  charged what a kill at the same moment is charged — the costliest step the
  run (or a candidate's parent) has paid for, marked as an estimate — so the
  budget ceiling stops a step that keeps being interrupted. When the
  interrupted step costs more than any step before it, each interruption still
  under-counts by the difference, so the ceiling stops it later than its real
  cost would; ADR-0007 states the bound.
- **A resumed run no longer forgets what its last step cost.** belay
  records a step's cost in the journal and then saves it in manifest.json.
  If belay was killed between the two, `belay resume` read the older total
  from manifest.json and let the run spend that step's cost again, past
  `budget.max_usd`. A resume now adds up the cost of every step in the
  journal, and never starts below the total manifest.json saved.
- **A step cut short by a kill -9 is no longer free.** When belay is killed in
  the middle of a step, nothing is left to say what that step spent, and
  `belay resume` used to count it as nothing, so a run killed during a paid
  step paid for it twice and counted it once. The step is now charged the most
  any one step of the run has cost so far, and the run's cost is shown as
  estimated from then on. A run killed in its first paid step, before anything
  was spent, is still charged nothing, because there is nothing to go on.
  `join`, `promote` and the fanout step call no agent of their own and are
  charged nothing; a candidate's own steps are charged as any run's are.
  ADR-0007 explains the choice.
- **Resuming a run kills the agent and test processes a kill -9 left
  running.** belay starts each of them in a process group of its own and kills
  that group when a step is cancelled or times out — but only while belay is
  alive to do it. After a kill -9, or a second Ctrl-C, they were left running:
  your terminal's Ctrl-C never reaches them, so an agent could keep working,
  and keep spending, with nothing watching it. belay now writes down every
  process it starts, before the process starts, in its own state directory,
  `$XDG_STATE_HOME/belay` or `~/.local/state/belay` — not in the run's
  directory, which in a run that edits in place is inside the workspace the
  agent writes — and `belay resume` kills whatever is still running from the
  step that was interrupted. It checks each process first — its start time,
  that it leads its own process group, and that it still runs the program
  belay started — so a process id that has since been given to something else
  is never signalled. belay now needs `HOME` or `XDG_STATE_HOME` set to start
  a step's processes.
- **An empty `NO_COLOR` no longer turns `belay timeline` plain.** The
  timeline switched colour off whenever `NO_COLOR` existed, even exported
  empty, while every other command waits for a non-empty value, as
  no-color.org asks. It now reads it the same way as the rest of belay.
- **The golangci-lint gate waits for golangci-lint instead of failing on
  it.** golangci-lint allows one instance per machine at a time, and one
  that finds another running waits about five seconds and then gives up with
  "parallel golangci-lint is running", which belay reported as a quality
  gate that could not run — failing the step for something unrelated to the
  code. Anything else running golangci-lint for more than five seconds, such
  as an editor's integration or another run, could cause it, and best-of-N
  would have caused it routinely: of five gates started together, one failed
  in each of three tries. belay now passes
  `--allow-serial-runners`, so a gate waits its turn. It does not pass
  `--allow-parallel-runners`, which would let concurrent instances hand each
  other findings through their shared cache.
- **A finding's path is relative to the workspace even when the workspace is
  reached through a symbolic link.** ruff reports a file by its resolved path,
  so in a workspace under macOS's `/var`, which is a link to `/private/var`,
  every finding was shown with its whole absolute path, and the review step
  could not tell the file was part of the workspace. The two paths are now
  also compared resolved.
- **A Python change that leaves a file unparsable fails the lint gate.**
  ruff 0.8 stopped reporting a syntax error as `E999` and reports it as
  `invalid-syntax`, which belay's severity table did not know, so it took the
  default, minor, and passed the gate at `review.fail_on: major`. It is a
  blocker now, as `E999` always was.
- **belay says why the lint gate has nothing to run, instead of claiming it
  found no project.** With `review.mode: lint` in a Python project that
  never configured ruff, belay refused to start because it "recognises no
  Go, Node or Python project" there, although it had recognised the project
  and would have run its tests. It now says it found a Python project with
  no ruff configuration, and that an empty `[tool.ruff]` table in
  `pyproject.toml`, or a `ruff.toml`, gives the gate ruff's default rules
  (judged only on the lines a run changes); or to set `review.mode` to
  `sonar`. A Node project without ESLint gets the same, and a directory with
  no project at all keeps the message it had.

### Security

- **Text belay did not write can no longer drive your terminal.** A plan, a
  goal, a note quoting an error, a file name and a directory name read off
  disk could all carry escape sequences, and `belay plan --no-input`,
  `belay timeline`, the narration and closing report of `belay run`,
  `resume`, `fork` and `queue`, and the last line belay prints on failure
  printed them as they were. A plan quoting a hostile repository could set
  your window title, write your clipboard (OSC 52), or move the cursor up and
  erase the line above it — even with colour off or output piped. Every
  control character in such text is now shown as a visible stand-in (ESC as
  ␛, a C1 control as `<9B>`), the way the plan viewer already showed it, by
  one rule every command shares. `belay runs`, `belay queue` and the
  timeline's file and candidate lists used to drop these characters instead,
  which could make one name read as another; they show the stand-in too. The
  `--json` output of `timeline`, `runs` and `queue list` keeps every string
  exactly as recorded, and now escapes the DEL and C1 controls that
  encoding/json left raw, so it is safe to print as well.

## v0.1.4 — 2026-09-17

### Fixed

- **The lint gate no longer passes code in a directory whose new files git
  ignores.** v0.1.3 checked only that HEAD holds something in the workspace.
  With a rule such as `ws/*` plus `!ws/.gitkeep`, HEAD holds `.gitkeep`, but
  git ignores every file a run adds there. Their findings were hidden and the
  gate passed. belay now also asks git whether a new file in the workspace
  would be ignored, and counts every finding if so.
- The docs now also list what else scoping by line misses: changes inside a
  nested repository or submodule, and new files git ignores by name, such as
  under a `*_gen.go` rule. The troubleshooting steps now show both
  golangci-lint commands belay can run.

### Security

- **Each step's agent now gets only the tools belay allows it.** belay passed
  a step's tool list to Claude Code as `--allowedTools`, which only
  pre-approves those tools. Every other built-in tool stayed available, and
  Claude Code's own settings could approve it: `defaultMode: acceptEdits`,
  for example, approves edits. So the plan step, meant to be read-only,
  could edit files, and the code and fix steps could run shell commands.
  MCP servers from the user's or the repository's Claude Code configuration
  also added their tools. belay now also passes `--tools`, which removes
  every other built-in tool. It also passes `--strict-mcp-config`, which
  loads no MCP servers except the ones belay configures itself. The code and
  fix steps therefore no longer run commands such as `go test` themselves;
  belay's test step runs the tests. This needs Claude Code 2.1.186 or later.

## v0.1.3 — 2026-09-17

### Fixed

- **The lint gate no longer passes code it cannot scope.** v0.1.2 always
  passed `--new-from-rev=HEAD`. In a directory the enclosing repository
  ignores, that hid every finding and the gate passed. A fanout candidate
  under `.belay` is such a directory, though belay doesn't lint candidates
  yet. belay now asks git first. If HEAD doesn't hold the
  directory, or there is no repository, commit or `git`, every finding counts.
  belay says so in a warning, shown with `--verbose`, and in the gate's
  summary, which the fix step passes to the agent.
- **A repository's `.golangci.yml` can no longer change what the gate
  counts.** belay now overrides the `issues.new`, `new-from-rev`,
  `new-from-merge-base`, `new-from-patch` and `whole-files` settings. Left in
  place, `new-from-merge-base` and `whole-files` counted findings committed
  before the run, and `new-from-patch` could hide the run's own findings.
- The docs now list what scoping by line misses:
  - findings a change causes on lines it didn't touch, such as unchecked
    errors where a function that now returns one is called;
  - files git ignores;
  - commits made during a run;
  - findings golangci-lint's cache takes from another checkout with identical
    code.

  They also correct two v0.1.2 claims: that a fanout candidate would have
  every finding counted, and that the warning golangci-lint prints when it
  can't scope reaches belay's output.

## v0.1.2 — 2026-09-17

### Fixed

- **The lint gate now judges the run's change, not the repository's
  history.** With `review.mode: lint`, belay ran golangci-lint over every
  package and counted every finding. In a repository with existing lint
  debt, every run failed the gate whatever it changed, and the fix step then
  asked the agent to repair findings in files the run never touched. belay
  now passes `--new-from-rev=HEAD`, golangci-lint's own option for reporting
  only findings on lines that differ from the last commit. New files count
  as changed. A package that does not compile still fails the gate, changed
  or not.

  The scoping needs git on `PATH` and a repository with at least one commit.
  Without them, golangci-lint prints a warning and reports every finding, as
  before. Uncommitted edits made before the run count as part of its change.
  ESLint and Ruff still report every finding.

## v0.1.1 — 2026-09-17

### Fixed

- **Claude subscription tokens now reach Claude Code.** belay passed
  `ANTHROPIC_API_KEY` to the `claude` child process but dropped
  `CLAUDE_CODE_OAUTH_TOKEN`, the token `claude setup-token` issues to a Claude
  Pro, Max, Team or Enterprise subscription. A user signed in that way saw
  every run fail at the plan step: `claude` exited 1 in about a second, at no
  cost and with no error text. belay now passes the token and redacts its
  value from the journal, logs and recorded cassettes, as it already did for
  the API key.
- **A failed `claude` run now says why.** When the CLI cannot start a run, for
  example because it is signed out or its credential was rejected, it prints
  the reason as an `is_error` result on stdout, writes nothing to stderr, and
  exits 1. belay reported only the exit code and the command line. It now
  reports the CLI's reason, such as `Not logged in · Please run /login`,
  redacted like everything else it records.
- A dry run reports whether `CLAUDE_CODE_OAUTH_TOKEN` is set, and the
  "not signed in" warning no longer appears when it is.

## v0.1.0 — 2026-09-06

First release. belay runs an autonomous coding agent as a state-machine graph
whose state is journaled after every step, so a crashed run resumes from the
last completed node instead of restarting.

### Added

- **`belay run`** — plan, approve, code, write, test, fix, review. Stops at the
  plan for a human to read, and prints its budget ceiling before it spends
  anything. `--dry-run` resolves everything and creates nothing.
- **`belay resume`** — continues a paused or crashed run. With no run id it
  picks the most recent resumable one and says which it picked.
- **`belay runs`** — lists this workspace's runs, newest first, with status,
  age, goal text and cost. A damaged run directory is listed as `unreadable`
  rather than silently omitted.
- **`belay timeline`** — one run's execution order, timing and per-node cost,
  with loop iterations numbered (`test #2/3`) so a fix loop is visible rather
  than inferred.
- **Durable, resumable runs.** An append-only NDJSON journal, fsynced per
  record. The dispatcher applies a node's state patch *before* journaling that
  it finished, so a crash re-runs a node rather than skipping one whose effects
  never landed.
- **Quality-gated loop.** A failing test suite and a failed quality gate both
  route back to `fix`, bounded by `graph.give_up`. golangci-lint, ESLint and
  ruff ship as gates; SonarQube is available via CLI or its MCP server.
- **Budget ceilings.** Token and USD spend is tracked per node and checked
  before each one. Estimated costs are marked as estimates.
- **Polyglot targets.** Go, Node and Python test runners and linters are
  detected automatically; any command can be configured explicitly.
- **Best-of-N.** `fanout` prepares N isolated candidate workspaces and `join`
  selects a winner by a configurable rule. Off by default.

### Known limitations

- **One agent backend.** Only Claude Code ships. `belay.AgentBackend` is
  exported and stable so others can be written, but none is implemented or
  tested (ADR 0005).
- **Unix only.** `internal/exec`, `internal/runner` and `internal/linter` are
  `//go:build unix`. Windows is not supported and no Windows artifact is built.
- **Promoting a fanout winner is manual.** Candidates are directory copies, so
  belay records which one won and leaves merging it to you (ADR 0004).
- **A cassette replays the agent, not the workspace.** Replay serves recorded
  responses; it does not reproduce the file edits the agent made, so a replayed
  run diverges as soon as a real test runner reads the workspace (ADR 0009,
  amended).
- **No published fix-rate figure.** `test/fixrate` is the instrument that would
  produce one against the seeded-defect catalogue; no number is claimed here.
