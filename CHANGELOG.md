# Changelog

All notable changes to belay are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and belay follows
[semantic versioning](https://semver.org/spec/v2.0.0.html) — with the caveat
that only `pkg/belay` carries a compatibility promise. Everything under
`internal/` may change in any release.

## Unreleased

### Added

- **Best-of-N works: a run with `fanout.enabled` runs its candidates and
  keeps the best.** Until now `fanout.enabled` changed nothing a run did —
  nothing in the graph led to the fanout step, and that step only prepared
  workspaces. A run that fans out now goes plan → approve → fanout → join:
  once the plan is approved, each of `fanout.candidates` candidates codes,
  tests, repairs and is reviewed in a workspace of its own, and `join` keeps
  the one `fanout.select` prefers among those whose tests passed. A branch
  run merges the winner onto its branch; an in-place run leaves it in its
  workspace, and the run's last lines name the directory to copy it back
  from. Each candidate is a run of its own, with its own journal, plan,
  blackboard and spend, under `.belay/runs/<run-id>/candidate-runs/`, so a
  crash or a Ctrl-C mid-fanout resumes like any run: finished candidates are
  kept, and only the interrupted ones run again. The new
  `fanout.concurrency` (default 3) says how many candidates run at once; it
  is read again on every resume, so lowering it backs off a rate limit.
  `budget.max_usd` is divided between the candidates before any of them
  starts, and `belay run --dry-run` shows each one's share, the route each
  candidate walks, how the winner is chosen, and how far past the ceiling a
  fanout can end: up to one step per candidate, because a candidate is
  checked before a step and not during one. `fanout.enabled`,
  `fanout.candidates` and `fanout.select` are recorded when a run starts and
  a resume keeps them, warning if `belay.yaml` has changed since. Several
  candidates running at once are told apart on screen by a `[cand-02]`
  prefix. `fanout.enabled` now requires `budget.max_usd` above zero, since
  there is otherwise no ceiling to divide, and is refused with
  `review.mode: sonar` or `ai`, whose SonarQube projects would be shared by
  every run's candidates; both configurations used to load and do nothing.
  A fanout leaves N workspaces and N candidate-run directories behind, and
  belay has no command to remove them yet. ADR-0019 records the decision.
- **`belay fork` re-runs one step of a run without paying again for the steps
  before it.** `belay fork <run-id> fix` makes a new run, with an id of its
  own, in a copy of your repository under `.belay/forks/`, from the state the
  run was in just before that step: it inherits everything before it, runs
  the one step, and pauses. Give a fix loop that gave up another round with a
  stronger model (`--model opus`, for plan, code and fix), or re-run a review
  after changing a setting, and pay for that step alone. `--visit 2` picks
  the second time the step ran, numbered the way `belay timeline` numbers
  them; without it belay takes the last. The original run is only ever read
  — not a byte of its journal, state or files changes — so both outcomes are
  there to compare. Carry the fork on with
  `belay --workspace .belay/forks/<fork-id> resume <fork-id>`, which belay
  writes out, and `belay runs` names every fork with the command that lists
  it. Before it spends anything, belay checks what it cannot put back: if
  your files have changed since the step ran it stops and says so, because
  that would be a different experiment, and `--current-files` re-runs the
  step on them as they are now; it names every setting that differs from
  the original, with both values; and the agent's conversation never carries
  over, since the original's went on past that step. The fork's spending
  starts at zero, and what its inherited history cost is recorded beside it
  rather than counted twice. `fanout`, `join` and `promote` cannot be forked,
  nor can a run recorded by a belay that kept no snapshots. `--dry-run`
  describes the fork exactly and creates nothing. ADR-0017 records the
  decision.

- **Line up goals and let belay work through them while you are away.**
  `belay queue add "<goal>"` adds a goal, and `belay queue start` works
  through the queue one goal at a time, each in a copy of the workspace made
  when belay reaches it, under `.belay/queue/work/`, so your checkout is never
  edited. Before it spends anything, a start says how many goals it will run,
  the most each can spend (`budget.max_usd`) and the product; it refuses
  `budget.on_exceed: warn`, since nobody is watching a queue to stop an
  overrun. A goal whose plan stops for review is parked, and the queue moves
  on to the next: `belay queue approve <goal>` releases it, and the next start
  carries it on. Only a goal you add with `--no-approval` skips the gate;
  belay never turns it off by itself. Ctrl-C pauses the goal in progress, and
  a crash, a kill or a reboot loses nothing: `belay queue start` again picks
  up the goal it had in hand, in the same run, and then the rest. The queue
  records each goal's claim before it makes anything for it, so no crash can
  run a goal twice. `belay queue` on its own is the morning report: what the
  last start did, what it spent, and the exact command for each goal that
  needs you; `belay queue list --json` is the documented shape for scripts,
  and `belay queue rm` takes back a goal nothing has started. Taking a
  finished goal's change from its copy is up to you. `belay runs` still
  prints the JSON it always did, and under its table names the runs that
  live in the queue's copies. ADR-0017 records the decision.
- **Guardrails a run's change must obey, whatever its tests say.** The new
  `policy` section in `belay.yaml` declares paths a change may not touch
  (`forbidden_paths`), ceilings on its size (`max_changed_files`,
  `max_changed_lines`), a ban on declaring a dependency the workspace did not
  already declare (`no_new_dependencies`), a rule that a change of one kind
  must bring a change of another with it (`required_tests`, typically a test
  beside the code), and text a changed file may not contain
  (`forbidden_content`). A guarded run gains a `policy` step between `write`
  and `test`, so the change is judged before anything else is spent on it and
  again on every pass of the fix loop. A forbidden path or an exceeded ceiling
  ends the run, because a change that touched what it may not touch is not
  something to ask an agent to repair; the other rules go to the fix loop,
  which tells the agent what it broke and asks for a repair, bounded by
  `graph.give_up`. Violations reach you as the same report a linter's findings
  do, archived per step, and `belay run --dry-run` prints every rule and says
  which ones would end a run. A rule belay cannot honour — an unusable path
  glob, a pattern that is not a regular expression — is refused when the
  configuration loads rather than skipped at the gate: a guardrail that
  silently does nothing is worse than none, because somebody is relying on it.
  The rules, and a survey of what your workspace declared, are recorded when
  the run starts, so `belay resume` holds a run to the rules it began with and
  warns if `belay.yaml` has changed since — a policy added mid-run has no
  record of the tree from before the agent ran, and could only fail the run
  for something it did not do. `max_changed_lines` counts the whole of every
  file the change touches rather than a diff, because belay keeps no copy of
  the tree from before; a one-line edit to a 900-line file counts 900.
  Best-of-N is not a way around a rule: `join` will not promote a candidate
  whose change broke one. Repositories that declare no policy run exactly as
  they did. ADR-0014 records the decision, and `docs/config.md` documents
  every setting.
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

- **Each step can call a model of its own.** `agent.models` routes a node to
  a model, so the strong one writes the plan and repairs a failure while the
  cheap one does the mechanical middle, and the same `budget.max_usd` buys a
  longer run. A node with no entry is sent `agent.model`, so a configuration
  without it costs exactly what it did. Only the nodes that call a model may
  be keyed — `plan`, `code` and `fix` — and any other name is refused when
  the configuration loads, by name and with the names it could have been: a
  typo that quietly sent the whole run to one model is the failure that would
  make routing not worth having. `belay run --dry-run` prints a row per node
  with the model it would use and that model’s list rate, and states no
  total, because belay cannot know how many tokens a goal will take and a
  wrong dollar projection is worse than none. Every model a run would use is
  checked against the backend before the run starts, not just `agent.model`,
  so one the backend cannot price stops the run before it spends rather than
  in the middle of it. Spend is recorded against the model that actually ran
  each step, so `belay timeline` shows the cheap step cheap.
- **A run works on its own branch and never edits your checkout.** In a git
  repository, `belay run` now creates `belay/<run-id>` at the commit you are
  on, checks it out in its own worktree under `.belay/runs/<run-id>/`, and
  works there. It commits once, after the quality gate passes, under your own
  git identity with a `Belay-Run: <run-id>` trailer. Your checkout keeps its
  branch, its commit and its uncommitted work, and the run's result is a
  branch you can read with `git log -p`, take with `git switch`, or delete.
  Outside a git repository, or with `output.mode: "in_place"`, belay edits
  the workspace directly, as it always did. The new `output.mode` setting
  chooses, and `auto` — the default — picks a branch whenever the workspace
  can take one and says why when it cannot. `belay resume` finishes a run the
  way it started, whatever `belay.yaml` says by then. ADR-0012 records the
  decision.
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
- **`belay plan` reads a finished run's plan.** Naming a run that is over used
  to be refused with "it is already finished" — which is exactly the run whose
  plan has just moved. `belay plan <run-id>` now shows it from wherever it is:
  the copy in your repository, the run's commit for a branch you have not
  taken yet, or the run directory when the plan never moved.
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
  plan step writes a new plan with your words in its prompt — then stops at the
  plan again so you can read it. Asking is a step of the run like any other: it
  is journaled, it counts against the budget, `belay timeline` shows it, and
  `belay resume` picks it up if it is interrupted. Neither form needs a
  terminal. `belay resume` still means "the plan as it stands is approved", so
  resuming after asking for a new plan withdraws the request instead of
  spending on one, and says so.

- **A plan can be read, edited and approved in belay, without leaving it.**
  At a terminal, `belay plan` now opens the plan in a full-screen viewer:
  scroll it, edit a step in place, add, delete and reorder steps, undo, ask
  the agent for a different plan in your own words, and approve it and carry
  on — all in one place, with what the run has spent shown before you commit
  it to spending more. `belay run` and `belay resume` open it too, whenever a
  run stops at its plan and there is a terminal to open it on. Press q to
  leave: the run stays exactly where it was waiting, and belay says how to
  come back.

  Nothing about belay without a terminal changes. The viewer opens only when
  belay is reading from and writing to a real terminal, `TERM` is set and is
  not "dumb", `CI` is unset, and `--no-input` — a new flag for saying so
  outright — was not given. Anywhere else, and under `--dry-run`, every
  command prints exactly what it printed before and exits, and the run waits
  for `belay resume` as it always has.

  What the viewer does is what you could already do by hand: it edits the
  same plan file, which it saves only when the approve gate would accept it,
  and approving is `belay resume` and asking for a new plan is `belay plan
  --regenerate`. It never runs a step of the run itself — it opens after the
  run is durably waiting and closes before belay dispatches again — so
  stopping belay while it is open loses nothing but edits you had not saved.
  ADR-0015 records the decision, including the nineteen modules it adds, all
  MIT or BSD-3-Clause.

- **A dry run says that `fanout.enabled` does nothing yet.** Nothing in the
  graph leads to the fanout node, so a run with fanout on takes the ordinary
  route once, whatever `fanout.candidates` says — and belay said nothing about
  it, while somebody who had turned it on was expecting N attempts and
  watching what N attempts would cost. `belay --dry-run run` now lists it
  under "worth knowing".
- **`belay timeline` lists the files a run changed.** After the totals, a
  "Files changed:" section lists every file the run changed, one per line
  and sorted, including the files the fix loop repaired. It is left out when
  the run has changed nothing. `belay timeline --json` carries the same list
  as `changed_files`. The README's example output showed this section before
  belay printed it, in a table layout belay never used; it now shows exactly
  what belay prints, and a test keeps it that way.

### Changed

- **A run that has met its budget ceiling still delivers the work it already
  bought.** The ceiling is checked before every node, which meant a run that
  reached `budget.max_usd` during its last coding step stopped just short of
  promoting the change it had paid for, and the only way to collect it was to
  raise the limit in `belay.yaml` and resume. `promote` runs git and calls no
  model, so stopping there saved nothing and lost everything; it is now
  exempt from the check, along with `join`. Every other node is checked
  exactly as before, and both nodes' spend is still recorded — what changed
  is what is checked, never what is counted.
- **`fanout.isolator` defaults to `auto`, and is a closed set.** It was free
  text checked only for emptiness, so `worktrees` or `dircopy ` loaded fine
  and failed later, when a run had already been created — while
  docs/config.md claimed `dircopy` was the only accepted value. It now
  accepts `auto`, `worktree` or `dircopy` and rejects anything else by name.
  The default changes from `dircopy` to `auto`, which keeps it in step with
  `output.mode`.
- **belay v0.1.x can no longer resume a run this version has saved.** belay
  ignores fields it does not know in a run's state.json and manifest.json,
  and saves the run without them. Once runs carry settings that change what
  they do, an older belay resuming one would drop those settings without a
  word. Both files now carry a `requires` list naming what a run depends on,
  and a belay that lacks something on the list refuses the run by name and
  says to upgrade. Nothing adds to the list yet. v0.1.x never reads it, so
  both files also move to schema version 2, which v0.1.x refuses: its
  `belay runs` shows such a run as "unsupported manifest version 2", and its
  `belay resume` does not offer it. Runs started by v0.1.x still resume, and
  are saved at version 2 from then on. With two belay installs, resume runs
  with the newer one. ADR-0011 records the decision.
- **A run belay stops at one of its own limits is paused, not ended.**
  Reaching `budget.max_usd` or `budget.max_tokens`, running out of
  `graph.max_steps`, or overrunning `graph.node_timeout` used to end the run
  for good: the only way on was a new run from nothing, which would spend the
  whole budget again from zero. belay now pauses the run and says which
  setting to raise. Raise it in belay.yaml and `belay resume <run-id>`
  carries on with everything the run has already spent still counted; leave
  it and the run stops at the same limit again, having done nothing and spent
  nothing. These limits are read from belay.yaml every time a run is
  resumed, unlike the shape of the graph, which is fixed when a run starts.
  A step that overran `graph.node_timeout` runs again from its start. The
  exit code is unchanged: an invocation belay stopped this way still exits 3,
  so scripts can tell it from a run that paused for you to read the plan.
  `belay runs` lists such a run as waiting and `belay resume` accepts it.
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

- **The policy gate judges what the workspace says changed, not what the
  agent says it changed.** It judged the agent's own report, so an agent
  that edited `infra/main.tf` and rewrote `belay.yaml` to `policy: {}` while
  reporting only `src/app.py` passed every rule ("1 file checked"), and a
  branch run committed both edits; a report spelled `Infra/main.tf` dodged
  `infra/**` too. belay now reads the change off the workspace: in a git
  repository, every file git says differs from the run's starting commit,
  committed, staged, edited, deleted or new, in its true name; in place in a
  git checkout, less what you had already changed before the run; with no
  git (including a queued goal's copy and an in-place run's candidates),
  every file that differs from a record of the workspace taken when the run
  starts. Files the agent changed and did not report are judged like the
  rest and named in the gate's verdict, `belay timeline` and the commit. On
  a disk that folds case, as macOS's does, rules match without regard to
  case. A fork, whose copy cannot say where its source's change began, is
  judged on the agent's report and says so in every verdict and in its dry
  run. `belay run --dry-run` says what the gate will judge.
- **A branch run commits exactly what was recorded and judged.** promote
  committed everything in the run's worktree, so an edit made there while a
  run was paused, or a file the test command wrote, went into the commit
  unjudged while the timeline said fewer files were verified. promote now
  pauses, naming the files, when the worktree holds anything the run's
  steps did not record, and in a guarded run judges the change against the
  policy once more before committing; remove what does not belong and
  `belay resume`. A run started in a subdirectory pauses the same way over
  a change above that directory.
- **A repository's `belay.yaml` can no longer drop your own guardrails.**
  Policy rules from a later configuration file replaced the earlier ones
  outright, so a project `belay.yaml` forbidding `secrets/**` silently
  discarded the `.github/workflows/**` and `infra/**` your
  `~/.config/belay/config.yaml` forbade, and the dry run showed only the
  project's rule. Policy now combines across files to the most restrictive
  result: forbidden paths, required tests and forbidden content are joined,
  each ceiling is the smallest any file sets (a later `0` does not lift
  one), and `no_new_dependencies` holds if any file sets it. Every other
  setting still takes the later file's value.
- **A policy pattern that describes a secret is no longer redacted as one.**
  `belay run` printed "redacted secret-shaped config values" on every run
  whose `policy.forbidden_content` held a pattern like the documented
  `AKIA[0-9A-Z]{16}`, or whose `policy.forbidden_paths` held a long path,
  and the manifest's record of the settings showed `REDACTED` where the rule
  should be. The rules are kept now; a value shaped like a credential under
  any other setting is still redacted.
- **belay starts on a Claude model its price table does not list.** It
  refused, before spending anything, any model outside the table it ships
  with: `claude-sonnet-4-5`, `claude-opus-4-1`, a dated identifier such as
  `claude-haiku-4-5-20251001`, and the aliases `sonnet[1m]` and `opusplan`.
  Dated identifiers and those aliases are now priced as the model they name.
  Any other model is priced at the most expensive rate belay knows, marked as
  an estimate, with a warning naming it, so the budget ceiling still holds
  and errs towards stopping early. The table is only used when the CLI does
  not report a call's cost itself.
- **`fanout.select: fastest` no longer depends on how a run was
  interrupted.** A candidate's time counted a step a Ctrl-C cut short but not
  one a crash cut short, so the same work could win or lose depending on
  which stopped it. Only the steps that finished count now.
- **A paused fanout no longer fails for good on a resume it cannot carry
  out yet.** Resumed after `budget.max_usd` was set to 0, a run that fans out
  ended failed, with every candidate it had paid for out of reach; so did one
  whose run directory could not be read at that moment. The first now pauses
  at exit 3 and says to set `budget.max_usd` above zero; the second pauses
  with the fault in its note, and resuming tries again.
- **A fanout whose candidates stop at a limit says so, and exits 3.** A
  candidate that met its share of `budget.max_usd`, `graph.max_steps` or
  `graph.node_timeout`, each of which applies to every candidate, left the
  run "waiting for you" with exit 0, no reason printed and no setting named,
  and resuming met the same limit for ever. The run now stops like any
  other limit: it names each candidate, why its own run stopped and the
  setting to raise, and a resume that raises nothing spends nothing. The
  run's spend, in `belay runs` and the closing report, now includes what its
  candidates have spent, at every pause and not only once the fanout
  finishes: it showed $0.10 of $1.00 where $1.10 had been spent.
- **After a crash mid-fanout, resuming collects the candidates without a
  raised ceiling.** The resume charged the killed fanout step the plan's cost
  a second time, then re-ran the launch-time cost projection against that
  inflated spend and refused to continue ("projected at $2.0000 each on top
  of $2.0000 already spent") until `budget.max_usd` was raised, which also
  raised the candidates' envelopes. Candidates already claimed are now
  adopted without the projection, at the envelopes they were given.
- **A crash inside `join`, `promote` or the fanout itself is no longer billed
  as the run's costliest step.** Resuming after a kill charges the step that
  died an estimate of what its agent may have spent, but these steps call no
  agent of their own: after a fanout the costliest step was the whole
  fanout, so a kill inside promote's merge reported $2.10 against $1.10
  spent, and a kill mid-fanout charged the plan again. They are now charged
  nothing; a candidate's own steps are still charged as before.
- **A step stopped by Ctrl-C or `graph.node_timeout` is charged for, so the
  budget ceiling stops a step that keeps being interrupted.** The agent CLI
  prints its bill only when it finishes, so a call belay stopped reported
  nothing and was recorded as free: four Ctrl-C and resume cycles of a
  fanout started ten $0.40 calls and reported $0.90, and a step that kept
  timing out was never stopped by the ceiling. Such a step is now charged
  what a kill at the same moment is charged — the costliest step the run (or
  a candidate's parent) has paid for, marked as an estimate — unless it
  reported its own spend. When the interrupted step costs more than any step
  before it, each interruption still under-counts by the difference, so the
  ceiling stops it later than its real cost would; ADR-0007 states the bound.
- **A Ctrl-C just after a candidate's workspace is made no longer loses that
  candidate.** A candidate whose run had been interrupted, or killed, before
  its first step resumed at "plan", a step a candidate does not have, and
  ended failed for good; the fanout then settled without it, and a Ctrl-C
  as the last copy finished could fail every candidate and the whole run. A
  candidate now starts at its own first step until its journal records one.
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
- **belay no longer takes a copy it made for the repository around it.** A
  queued goal's copy lives under the repository's `.belay`, and git finds the
  repository a few directories up, so `belay --workspace <copy> resume` —
  the command `belay queue` prints for a goal stopped at a limit — decided to
  branch the person's own repository, and warned that `belay.yaml` now asked
  for a branch when nobody had changed it. A workspace under `.belay` now
  edits in place, says why, and refuses `output.mode: branch`.
- **`belay fork` works when `belay.yaml` enables fanout.** A fork re-runs one
  step of a single run, and is meant to turn fanout off as it turns branch
  output off, but it kept `fanout.enabled`. Its graph was then the fanout
  graph, which has no `code`, `test`, `fix` or `review`, so forking any of
  them ended the new run as failed with `unknown node`. A fork is now always
  a single run.
- **A fork or a queued goal can be carried on when `belay.yaml` says
  `output.mode: branch`.** Both run in place, in a copy under `.belay`, and
  the copy can never take a branch, so resolving it refused `branch` — and
  that refusal outlived the run's own pin, which says in place. belay
  refused the very command it had printed: `belay --workspace <copy> resume
  <fork>` after a fork, and the resume `belay queue` prints for a goal
  stopped at a limit. A resume now judges the output the run was started
  with, as `docs/config.md` promises: what `belay.yaml` would give a new run
  is a warning, never a refusal. The same rule stops a resume from warning
  about uncommitted work a branch run starting now would leave out, and from
  telling a branch run that `belay.yaml` "now says" a mode it does not.
- **A fanout whose isolator cannot serve its output is refused before it
  spends, and `fanout.isolator` no longer refuses a run that does not fan
  out.** `fanout.isolator: worktree` in a run that edits in place passed the
  dry run, paid for the plan, and then could not make a single candidate's
  workspace, so the fanout paused, and paused again on every resume. It is
  now refused up front, like `dircopy` in a branch run. That refusal, in turn,
  was applied to every branch run, fanout or not, so a repository whose
  `belay.yaml` set `dircopy` as earlier docs advised could not start a run at
  all; both refusals now apply only to a run that fans out, and a resume
  judges them against the output and fanout the run was started with.
- **`belay queue` says where a fanout goal's change is.** A queued goal whose
  run fans out leaves its copy as it was claimed: the change is in the
  winning candidate's own workspace, under the run's `candidates/`, because
  belay does not merge a run that edits in place. The queue said the goal
  "finished in its copy" and to take what you want from there, which found
  nothing. It now names the winner and its workspace.
- **A failed fanout run names a candidate only when one exists.** Its last
  lines offered `belay timeline <run> --candidate cand-01` whenever the run
  fanned out, including a run that failed at its plan, before any candidate
  was made, where that command says the run has no candidate runs. The line
  now appears only when a candidate run is on disk, and names the first one.
- **A queued goal's plan always waits for you unless you added it with
  `--no-approval`.** `graph.approval: false` anywhere — the project's
  `belay.yaml`, or your own `config.yaml`, which reaches every repository —
  took every queued goal from its plan to its code unattended, and
  `belay queue add` and `queue start` blamed `belay.yaml` for it. The queue no
  longer reads `graph.approval`: the gate stops every goal except one added
  with `--no-approval`.
- **The command belay prints to remake a deleted run worktree works.** A
  branch run whose worktree directory was deleted is refused with the git
  command that makes it again, and git refused that command: the worktree
  was still registered, and git takes the path back only with `worktree add
  -f`. belay now prints `-f` when git still has the worktree registered, a
  plain `worktree add` when it does not, and quotes the paths.
- **Forking a fix loop that gave up calls the model.** `belay fork <run-id>
  fix --model opus`, the README's own example, re-runs the last fix — the one
  that gave up — whose state had every round of `graph.give_up` used, so the
  fix step gave up again without calling anything, and `--model` changed
  nothing. A fork of such a fix now gets one more round, and says so with the
  other differences before it spends anything; nothing in `belay.yaml` needs
  to change.
- **belay says what it warns about on real runs, not only under `--dry-run`.**
  Uncommitted work a branch run leaves out, a resumed run's settings
  disagreeing with `belay.yaml`, a credential belay cannot see: each was
  collected for every run and printed only by `--dry-run`, so the run itself
  said none of it. `belay run`, `belay resume`, `belay fork` and each queued
  goal now print them on stderr, after the announcement and before anything
  is spent.
- **`belay queue` shows a goal carried on by hand as its run now stands.**
  For a goal stopped at a limit, the queue says to raise the limit and run
  `belay --workspace <copy> resume <run-id>` yourself. After that run
  finished, `belay queue`, `queue status` and `queue list` still said
  "paused at a limit" and offered the same resume, which then answered that
  the run is already finished. The reports now read each goal's run, and show
  one that has finished, failed or stopped since the queue recorded it that
  way.
- **A repair's file list is checked against the disk, not taken on trust.**
  The fix step routed straight back to the tests, so a repair was the one
  change in a run that nothing verified: no claimed path was proven to be a
  regular file inside the workspace, no diff of the repair was recorded, and
  the run's `last_diff` still pointed at the original change however many
  repairs had happened since. A repair now goes back through the `write` step,
  which does all three.

- **A step that stops because of the budget ceiling now pauses the run, like
  every other limit.** belay's own guards pause a run that reaches
  `budget.max_usd`, so raising the ceiling and resuming continues it and keeps
  what it has already spent. A step that refused for the same reason — it
  could see the ceiling would not cover what it was about to buy — ended the
  run instead, although belay then told you to raise the setting and resume,
  which could not work. Such a step now pauses the run and is run again on
  `belay resume`, against the ceiling as it is then. The exit code is
  unchanged: the invocation still exits 3.

- **`belay runs` says "needs plan approval" only when the plan gate asked.**
  A run stopped by Ctrl-C while the gate was running, or by one of belay's
  own limits just before it, is paused at the same step and looked the same
  in the listing, which reads manifest.json — so belay told you to go and
  read a plan nothing had offered you. The listing now asks the run's journal
  whether the gate stopped to ask, which is the same question `belay resume`
  asks before treating a resume as your approval, and says "waiting at
  approve" for the rest. `belay runs --json` is unchanged in shape.

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
- **Stopping a run with Ctrl-C no longer costs it a step.** The interrupted
  step was counted against `graph.max_steps` before the run was paused, so a
  run stopped twice had two fewer steps left to finish in than one killed
  twice — although `belay run`'s help promises that Ctrl-C leaves the run
  where a kill would. It also moved the re-run's evidence directory under
  `nodes/` beside the attempt it replaces rather than over it. Only a step the
  run acted on is counted now.
- **A version-1 run's manifest is now saved at the current schema version
  before belay writes anything else to the run.** Resuming a run belay v0.1.x
  started upgrades both of its documents, and state.json was saved first — by
  the dispatcher's first patch, or by `belay resume` recording your approval.
  A crash in between left a version-1 manifest over a version-2 state.json,
  which a released belay accepts: it would resume the run, append to its
  journal, and on the next invocation report that the run had already ended.
  The manifest is now saved first, which is the version a released belay
  refuses (ADR-0011).
- **A run that requires something this belay does not have is refused before
  its journal is touched.** A run may declare a required capability in
  state.json alone, and the dispatcher read manifest.json, opened the journal
  and healed a crashed step before it ever read state.json — so a run it was
  not allowed to read had already been written to, and the first guard it met
  ended it for good. Nothing sets `requires` yet, so no run was affected in
  practice.
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
- **Files changed in the fix loop are now recorded with the rest of the
  run's change.** belay keeps a list of the files a run changed, taken from
  the files the agent reports after writing the code. When a test or the
  quality gate failed and the agent repaired it, the files that repair
  touched were never added, so the list belay passes to a reviewer, and
  shows the agent on its next repair, left them out. belay now asks the
  agent which files each repair changed and adds them to the list, each
  file once. A repair that reports a path outside the repository fails the
  run, as the first change already did.
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
- **belay no longer tells you to resume a run it cannot resume.** After
  `graph.max_steps`, `graph.node_timeout`, or the budget ceiling with
  `budget.on_exceed: abort`, the closing report offered `belay resume
  <id>`, which refuses such a run. It now says the run has ended, names the
  setting to raise — `budget.max_usd` or `budget.max_tokens` for the budget
  ceiling — and gives the command that starts a new run. A run aborted
  any other way is pointed at `belay timeline`.
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
  are only warnings, when they are errors, and that environment variables
  and flags override settings, when only a `--config` file does. Tests now
  check every documented setting's type, default and valid values against
  the code, and fail when a setting has no entry.
- **The docs no longer say fanout works.** The README said parallel
  candidate generation works and only merging the winner back was manual,
  and docs/architecture.md said the fanout node runs each candidate from the
  code step onward. Neither was true. The fanout and join nodes exist and
  are unit-tested, but no step in belay's graph leads to fanout, so no run
  reaches either one, and `fanout.enabled: true` changes nothing a run does.
  The fanout node only prepares a workspace per candidate; nothing runs a
  candidate. The README, docs/architecture.md, docs/troubleshooting.md,
  docs/config.md, docs/adapters.md and examples/naive now say so. Running
  candidates is planned, and ADR-0019 is reserved for it.
- **A step that fails, times out or is stopped is charged for what it
  spent.** When the agent reported spending money and the step then
  returned an error, ran past `graph.node_timeout`, or was stopped with
  Ctrl-C, belay recorded the step as free: the money was left out of the
  run's total and out of the journal. A run could then go past
  `budget.max_usd`, and a run stopped with Ctrl-C and resumed could spend
  its whole budget a second time. Whatever the agent reported spending is
  now added to the run's total and written to the journal, however the step
  ended.
- **A resumed run no longer forgets what its last step cost.** belay
  records a step's cost in the journal and then saves it in manifest.json.
  If belay was killed between the two, `belay resume` read the older total
  from manifest.json and let the run spend that step's cost again, past
  `budget.max_usd`. A resume now adds up the cost of every step in the
  journal, and never starts below the total manifest.json saved.
- **A step cut short by a kill -9 is no longer free.** When belay is killed
  in the middle of a step, nothing is left to say what that step spent, and
  `belay resume` used to count it as nothing, so a run killed during a paid
  step paid for it twice and counted it once. The step is now charged the
  most any one step of the run has cost so far, and the run's cost is shown
  as estimated from then on. A run killed in its first paid step, before
  anything was spent, is still charged nothing, because there is nothing to
  go on. ADR-0007 explains the choice.
- **Resuming a run kills the agent and test processes a kill -9 left
  running.** belay starts each of them in a process group of its own and
  kills that group when a step is cancelled or times out — but only while
  belay is alive to do it. After a kill -9, or a second Ctrl-C, they were
  left running: your terminal's Ctrl-C never reaches them, so an agent could
  keep working, and keep spending, with nothing watching it. belay now
  writes down every process it starts, before the process starts, and
  `belay resume` kills whatever is still running from the step that was
  interrupted. It checks each process first — its start time, that it leads
  its own process group, and that it still runs the program belay started —
  so a process id that has since been given to something else is never
  signalled.
- **An empty `NO_COLOR` no longer turns `belay timeline` plain.** The
  timeline switched colour off whenever `NO_COLOR` existed, even exported
  empty, while every other command waits for a non-empty value, as
  no-color.org asks. It now reads it the same way as the rest of belay.
- **Ctrl-C while `belay fork` is copying your tree stops it cleanly.**
  `belay fork` says to press Ctrl-C once to stop it, but until its step
  started running, a Ctrl-C killed belay outright, part-way through copying
  the tree, and left a half-made copy under `.belay/forks` that `belay runs`
  listed as unreadable. A Ctrl-C during the copy now cancels it, removes
  what was copied, and says nothing was kept or spent.

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
- **A chain of symlinks can no longer lead out of a copy belay makes.** The
  copy a queued goal, a fork or an in-place candidate works in left out any
  symlink whose text pointed outside it, but judged each link's text alone.
  So `a/b/c/up -> ../../..`, which lands on the copy's root, was kept, and
  so was `esc -> a/b/c/up/../../../..`, which reads as the root too — yet
  the system follows `up` first and then climbs four levels above the copy,
  and in a queued goal's copy that is your own checkout. Once a copy is
  built, belay now follows every link it recreated through every link along
  the way and removes any that does not end inside the copy, loops included.
- **A run's files can no longer make `belay resume` kill a process.**
  `belay resume` kills the processes a killed belay left running, from
  records it kept in the run's `nodes/` directory — which, in a run that edits
  your workspace in place, is inside the workspace the agent writes. A
  record written there naming any process of yours, with its real start
  time, had belay send it SIGKILL. The records now live in belay's own state
  directory, `$XDG_STATE_HOME/belay` or `~/.local/state/belay`, and a process
  is killed only if it also leads its own process group and still runs the
  program the record names. A record found in a run's `nodes/` directory is
  reported and removed, never acted on: if you resume, with this belay, a run
  a development build of belay was killed during, kill what it left running
  yourself (`ps` lists it). belay now needs `HOME` or `XDG_STATE_HOME` set to
  start a step's processes.

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
