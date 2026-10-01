# Troubleshooting

<!--
Insertion markers. Each "slice:NAME" comment below marks where the work in
flight for that slice adds its text: insert directly above the marker and
leave the marker in place, so changes built in parallel land on different
lines. Delete a marker once its slice has merged.
-->

This document covers common failure modes, their symptoms, and remedies.

## The Agent Edited Nothing

**Symptom**: The run completed but no files changed, or the generated code is empty.

**Possible Causes**:
- The prompt was too vague or the goal was unachievable
- The agent backend did not understand the task (read what it was asked in `.belay/runs/<run-id>/nodes/<step>-code/prompt.txt`)
- The working directory was wrong (verify WorkDir in logs)
- The agent ran out of budget mid-task

**Diagnosis**:
1. Run `belay timeline <run-id>` to inspect the journal
2. Look for a code step whose note says the agent `reported no change`
3. Read the agent's reply, including any error it reported, in `.belay/runs/<run-id>/nodes/<step>-code/response.json`
4. Verify the goal in manifest.json is what you intended

**Remedy**:
- If the goal is unclear, start a new run with a clearer one: `belay run "what you want built"`. belay.yaml has no prompt setting; the goal is what the agent is given
- If the agent ran out of budget, increase budget.max_usd and resume the run
- If the working directory or model is wrong, edit belay.yaml and start a new run; `belay resume` does not re-run a run that completed

## Run is Paused

**Symptom**: `belay run` stops after the plan and says it "is waiting for you", with the path to the plan and a `belay resume <run-id>` command.

**Cause**: The approve node paused the run awaiting human approval.

**Diagnosis**:
1. Run `belay timeline <run-id>`: the approve step shows as paused, followed by "run paused here"
2. The approve step's note names the plan to read and the command that continues the run

**Remedy**:
1. Read the plan at `.belay/runs/<run-id>/artifacts/plan.md`, and edit it if you want to
2. If approved, run `belay resume <run-id>` to continue; resuming is the approval
3. If not approved, start a new run with `belay run "what you want built"`. The paused run stays as it is

## Test Failed; Agent Cannot Fix It

**Symptom**: The test node found failing tests, the agent tried to fix them with `fix` node, but the fixes failed the gate or produced empty output.

**Possible Causes**:
- The test failure message was unclear or incomplete
- The agent does not have enough turns to fix the issue
- The fix loop hit its max retry limit (graph.give_up)
- The problem requires external setup (a database, a service) the agent cannot reach

**Diagnosis**:
1. Read the failing tests in the test step's report, `.belay/runs/<run-id>/artifacts/test-<step>.json`
2. Read what each repair was asked in `.belay/runs/<run-id>/nodes/<step>-fix/prompt.txt`, and what it answered in `response.json` beside it
3. Count the repairs: `belay timeline <run-id>` numbers them, as `fix #1/3`. A node_finished record's attempt number counts re-runs after a crash, not repairs
4. Check if the run hit the step limit (ErrMaxSteps, graph.max_steps: 60 node executions by default)

**Remedy**:
- If tests require external services, set them up and retry
- If the agent needs more context, start a new run with a more specific goal
- If the loop is infinite, increase graph.give_up (default 3) to allow more fix attempts
- If the limit is reached, break the loop by reducing the goal to a smaller, more achievable task

## Toolchain Missing

**Symptom**: The run fails when it reaches the step that needs the tool, usually test or review, and the error belay prints names it: `belay: toolchain "golangci-lint" not found`. The steps before it, plan and code among them, have already run and spent.

**Cause**: A required binary (go, pytest, golangci-lint, sonar-scanner, etc.) is not on PATH.

**Diagnosis**:
1. The error message names which toolchain is missing (e.g., "golangci-lint")
2. Run the same command in a shell to see what the error is: `which <tool>`

**Remedy**:
- Install the toolchain and start a new run. The run that hit the missing toolchain has failed, and `belay resume` refuses a failed run
- Or, if using a different test runner or linter, update belay.yaml (test.runner, review.mode)

**Note**: This is distinct from "test failure". A missing toolchain returns an error and refuses to proceed, forcing human intervention. A test failure returns a TestReport and automatically triggers the fix loop. Belay makes this distinction so the remedy is clear: install software vs. edit code.

## Claude Exits Immediately

**Symptom**: The plan node fails with `claude failed with exit code 1` about a second after starting, and the run cost $0.00. From 0.1.1, belay adds the CLI's own reason, such as `Not logged in · Please run /login` or `Failed to authenticate. API Error: 401 Invalid bearer token`. On 0.1.0 there is no reason, only the command line. Running `claude` yourself works.

**Cause**: `claude` started without a credential. belay starts it with a deny-by-default environment: `HOME`, `LANG`, `PATH`, `TERM`, `TMPDIR` and `USER`, plus `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN`. Your shell passes everything, so a credential that lives in any other variable works there and not under belay. belay 0.1.0 also dropped `CLAUDE_CODE_OAUTH_TOKEN`.

**Diagnosis**:
1. Run `belay version`. On 0.1.0, a subscription token is never passed.
2. Reproduce what belay's child sees. This makes one short call if it succeeds:
   ```bash
   env -i HOME="$HOME" LANG="$LANG" PATH="$PATH" TERM="$TERM" TMPDIR="$TMPDIR" USER="$USER" \
     ${ANTHROPIC_API_KEY:+ANTHROPIC_API_KEY="$ANTHROPIC_API_KEY"} \
     ${CLAUDE_CODE_OAUTH_TOKEN:+CLAUDE_CODE_OAUTH_TOKEN="$CLAUDE_CODE_OAUTH_TOKEN"} \
     claude -p "reply ok" --output-format json < /dev/null
   ```
   If this fails and plain `claude -p "reply ok"` works, the credential is in a variable belay does not pass.

**Remedy**:
- On a subscription: upgrade to belay 0.1.1 or later, run `claude setup-token`, and export `CLAUDE_CODE_OAUTH_TOKEN`
- With an API key: export `ANTHROPIC_API_KEY`
- Or sign in interactively (`claude`, then `/login`) so the login is stored under `HOME` or, on macOS, in the Keychain
- Bedrock, Vertex, and proxy variables are not passed yet, so those setups do not work under belay

## Budget Exceeded

**Symptom**: The run stops with "belay: budget exceeded: spent $X of $Y limit", and belay says to raise the setting and resume.

**Cause**: Cumulative cost of all agent invocations crossed the configured ceiling.

**Diagnosis**:
1. Check budget.max_usd and budget.max_tokens in belay.yaml
2. Look at Usage.USD fields in journal records to see which invocations cost the most
3. Check if Usage.Estimated = true; estimated costs are lower-confidence

**Remedy**:
With `budget.on_exceed: abort` the run is paused, not ended. Raise the ceiling in belay.yaml and run `belay resume <run-id>`: belay reads the limits again as it starts, and the run continues with what it has already spent still counted, rather than spending it over again from a new run. The same holds for `graph.max_steps` and `graph.node_timeout`.
- Increase budget.max_usd in belay.yaml (if the project is worth it)
- Use a cheaper model (adjust agent.model in belay.yaml)
- Scope the goal to fewer changes (smaller diffs cost fewer tokens)
- If costs are overestimated, they may drop in a future API pricing update

## Interrupted or Crashed Run

**Symptom**: You pressed Ctrl-C, or `belay run` or `belay resume` was killed, lost its terminal or exited part-way, and a run directory exists at `.belay/runs/<run-id>/`.

**Expected Behavior**: The run can be continued. The first Ctrl-C (or a SIGTERM) pauses it: belay closes the step it was in, records the pause, says "belay stopped because you asked it to", and exits with code 3. A second Ctrl-C, a kill or a crash stops belay at once and leaves that step open, and `belay resume` closes it on the way back in. Either way, resuming runs the interrupted step again from its start, and does not run the steps that had finished.

**Diagnosis**:
1. Run `belay runs`: an interrupted run is listed as paused (after Ctrl-C) or running (after a crash), and both can be resumed
2. Run `belay timeline <run-id>` to see how far the run got. The interrupted step shows as stopped after Ctrl-C, or as interrupted after a crash
3. Look at the journal's final record: if it is run_completed, run_failed or run_aborted, the run has ended and cannot be resumed; anything else can be

**Remedy**:
1. Run `belay resume <run-id>` to continue from where it stopped, or `belay resume` for the most recent run still waiting
2. The journal file is automatically healed on resume (any torn tail is discarded)
3. If the same crash happens repeatedly, it may indicate a stuck node (increase graph.node_timeout in belay.yaml)

**Note**: A step cut off part-way may already have spent money that the agent had not yet reported. The run's recorded cost leaves it out, as it does for a crash.

## Fanout Does Not Follow belay.yaml on Resume

**Symptom**: You changed `fanout.enabled`, `fanout.candidates` or `fanout.select` and resumed a run, and the run kept doing what it did before. belay printed a warning that the run's fanout has changed in belay.yaml since it started.

**Cause**: Whether a run fans out, with how many candidates and how its winner is chosen, is recorded in the run's `manifest.json` when the run starts, and a resume walks the run the way it started. Changing the count mid-run would rename the candidates already claimed and re-divide money already promised to them; changing the strategy could crown a different winner than the one the interrupted attempt was choosing. A run started before your belay could run candidates has no such record, so it stays a single attempt.

**Remedy**:
- Start a new run for the new settings
- `fanout.concurrency` is the exception: it is read on every resume, so lowering it and resuming backs off a rate limit or a busy machine

## A Fanout Run Paused at fanout

**Symptom**: The run stops with `belay paused` at `fanout`, and its note says some candidates could not be finished, naming each and why (for example `no space left on device`, or a git lock).

**Cause**: Something a resume could fix stopped those candidates before they reached an end state. belay does not record a verdict about a candidate that never got the chance to run, so the fanout pauses rather than settling with fewer candidates than you asked for.

**Remedy**:
- Fix what the note names (free disk space, remove a stale `index.lock`), then `belay resume <run-id>`
- Resuming does not repeat finished work: the candidates that finished are kept as they are, and only the unfinished ones run. `belay timeline <run-id>` shows every candidate, and `belay timeline <run-id> --candidate <id>` opens one candidate's own run

## A Fanout Run Finished but My Files Did Not Change

**Symptom**: An in-place run with `fanout.enabled: true` completed and named a winner, but your checkout does not contain the change.

**Cause**: An in-place fanout does not merge. Each candidate worked in a directory copy of its own, under `.belay/runs/<run-id>/candidates/<candidate-id>/`, and belay has no branch to merge the winner from. Only a run that delivers on a branch (`output.mode: branch`) merges its winner.

**Remedy**:
- The run's next steps name the winner's workspace: review the change there and copy back what you want to keep
- Use `output.mode: branch` in a git repository, and belay merges the winner onto the run's branch for you

## A Fanout Run Is Refused Over review.mode

**Symptom**: `belay run` refuses to start, saying the run fans out and `review.mode` is `"sonar"` or `"ai"`.

**Cause**: Both name the SonarQube project after the directory they review, and every candidate's directory is called `cand-01`, `cand-02` and so on, in every run and every repository. Candidates would share SonarQube projects with every other run's candidates, and each quality gate would read results it did not produce.

**Remedy**:
- Use `review.mode: lint` for a run that fans out, or turn `fanout.enabled` off to review a run with SonarQube

## SonarQube Connection Failed

**Symptom**: The review step fails with `belay/sonar: SONAR_TOKEN is not set in the environment`, `belay/sonar: SONAR_HOST_URL is not set in the environment`, or `belay/sonar: docker (docker runner) did not produce a quality gate verdict (exit 1)` followed by the scanner's last error line. With `review.sonar.runner: binary` the tool is `sonar-scanner` instead of `docker`.

**Cause**: 
- SonarQube server is unreachable (wrong URL, network down, firewall)
- Authentication token is missing or incorrect
- Docker is not installed (in Docker mode)

**Diagnosis**:
1. Check that review.mode in belay.yaml is "sonar", and which runner review.sonar.runner names: "docker" (the default) or "binary"
2. Verify SONAR_TOKEN is set, and SONAR_HOST_URL is the server's URL
3. With the docker runner, verify `docker` is on PATH and the daemon is running
4. Try connecting manually: `curl -H "Authorization: Bearer $SONAR_TOKEN" $SONAR_HOST_URL/api/system/status`

**Remedy**:
- Set the SONAR_TOKEN and SONAR_HOST_URL environment variables
- With the docker runner: install Docker, or set review.sonar.runner to "binary" and install `sonar-scanner`
- If the server is temporarily down, start a new run once it is back. The run whose review could not reach it has failed, and `belay resume` refuses a failed run
- If authentication fails, rotate your token and retry

## Quality Gate Failed

**Symptom**: Review node returns a report with Gate="fail"; run branches to the fix node.

**Expected Behavior**: This is not an error; it is the design. The run tries to fix the issues automatically.

**Diagnosis**:
1. Read the issues in the review step's report, `.belay/runs/<run-id>/artifacts/review-<step>.json`
2. Check the Severity of each issue and compare against review.fail_on
3. The review step's note in `belay timeline <run-id>` names the tool that produced the report (`source=`) and the report's path

**Remedy**:
- If the fixes are working, wait for the fix loop to complete
- If the loop is stuck (same failures after many attempts), the issue may be unfixable by code edits alone (e.g., a missing type import from a broken dependency)
- If the gate threshold is wrong, edit review.fail_on in belay.yaml and retry
- If the underlying tool (golangci-lint, sonar-scanner) is misconfigured, update .golangci.yml or sonarqube configuration and retry

## Quality Gate Fails on Code the Run Did Not Touch

**Symptom**: With `review.mode: lint`, every run fails the gate whatever it changed, and the fix node asks the agent to repair findings in files the run never edited.

**Cause**: The gate is counting findings that were already in the repository. From 0.1.2, golangci-lint reports only findings on lines that differ from the last commit. That needs `git` on `PATH`, a commit that holds the workspace, and a workspace whose new files git doesn't ignore. belay checks all three first: from 0.1.3 the first two, and from 0.1.4 the third. If any is missing, every finding counts. belay then logs `cannot scope golangci-lint to the run's change; every finding counts` with the reason, and the gate's summary contains `not scoped to the run's change`.

This happens:
- outside a repository;
- in a repository with no commits;
- in a directory the repository ignores or hasn't committed yet;
- in a workspace where git ignores new files at the top level, even if it tracks its subdirectories. An example is a `.gitignore` with `/*` and `!/internal/`. Every finding counts there, committed ones too.

belay 0.1.1 and earlier always counted every finding, and ESLint and Ruff still do.

**Diagnosis**:
1. Run `belay version`. On 0.1.1 or earlier, every existing finding counts.
2. Look for the warning above in the output of `belay run --verbose`. Or look for `not scoped to the run's change` in the run's review report, `.belay/runs/<run-id>/artifacts/review-<step>.json`.
3. In the workspace, run the checks belay makes:
   ```bash
   git ls-tree --name-only HEAD                                     # must list files
   git check-ignore --no-index -q -- belay-scope-check.go; echo $?  # must print 1
   ```
   If the first fails or prints nothing, or the second prints anything other than `1`, belay can't scope the findings. Don't use `-v` here: it also prints negation rules, such as `!*.go`, for files that aren't ignored.
4. Reproduce what the gate sees. When belay can scope the findings:
   ```bash
   golangci-lint run --new=false --new-from-rev=HEAD --new-from-merge-base= --new-from-patch= --whole-files=false ./...
   ```
   When it can't, it counts everything:
   ```bash
   golangci-lint run --new=false --new-from-rev= --new-from-merge-base= --new-from-patch= --whole-files=false ./...
   ```

**Remedy**:
- Upgrade to belay 0.1.3 or later. 0.1.2 scopes findings, but lets a repository's `.golangci.yml` `issues` settings change the scope
- Run belay in a git repository with `git` on `PATH`, and commit the workspace first. If the workspace is in a directory the repository ignores, run belay from a repository that tracks it
- Commit or stash unrelated edits before the run: uncommitted changes count as part of the run's change
- If a finding comes from `typecheck`, fix the build first. A package that does not compile fails the gate whether or not the run changed it
- For ESLint and Ruff, fix or exclude the existing findings in the tool's own configuration, or raise `review.fail_on`, for example to `critical`

## Quality Gate Passes Although golangci-lint Reports Findings

**Symptom**: `golangci-lint run ./...` reports findings in the workspace, but the run's lint gate passed.

**Cause**: From 0.1.2, the gate counts only findings on lines that differ from HEAD. That leaves out findings committed before the run, which is deliberate, and five kinds it misses:
- A finding the change causes on a line it didn't touch, such as an unchecked error where a function that now returns one is called. Compile errors are the exception: they always count
- A finding in a new file git ignores, whether git ignores its directory or its name (for example a `*_gen.go` rule)
- A change inside a nested repository or submodule whose files are part of the module's packages. The enclosing repository's git never lists those files
- A change committed while the run was in progress
- A finding golangci-lint's cache took from another checkout holding identical code. The cache stores the other checkout's paths, and the scoping drops them ([golangci/golangci-lint#3502](https://github.com/golangci/golangci-lint/issues/3502))

**Diagnosis**:
1. Run both commands from step 4 in the section above, and compare their findings. A repository's own `.golangci.yml` can scope a plain `golangci-lint run ./...`, so use the second command, not that one
2. Check whether the finding's file is ignored: `git check-ignore -q <file> && echo ignored`
3. Check whether the file is inside a nested repository: `git -C <its directory> rev-parse --show-toplevel` prints a different directory than it does in the workspace
4. Check for commits made since the run started: `git log`

**Remedy**:
- Fix a finding the gate missed by hand, or start a run whose goal names it
- If you suspect the cache, run `golangci-lint cache clean`, then check again
- Don't commit while a run is in progress

## Node Timeout

**Symptom**: Error message: "graph: node timeout exceeded" and run aborts.

**Cause**: A single node took longer than graph.node_timeout (default 15 minutes).

**Diagnosis**:
1. Check which node timed out (look at the journal's last node_started record)
2. The timeout is likely due to a slow operation: test suite, code generation, sonar-scanner on a large repo

**Remedy**:
The run has ended, and `belay resume` will not continue it. Change belay.yaml, then start a new run:
- Increase graph.node_timeout in belay.yaml
- Or reduce the scope (fewer tests, smaller code changes, scoped SonarQube analysis)
- If timeouts happen repeatedly on the same node, there may be an infinite loop or a genuine performance problem

## Invalid Result

**Symptom**: Error message: "invalid result: ..." with details about what failed validation.

**Cause**: A node returned a Result that the dispatcher could not process.

**Expected Behavior**: This is a bug in the node implementation, not a user error.

**Diagnosis**:
1. Look at the error message; it says what was invalid: `Status is unset`, or `Status is OK but Next is empty (use graph.End to finish)`
2. This typically happens only during development or if belay itself has a bug

**Remedy**:
- Report the error to the belay team with the full journal
- If you wrote a custom node, review your Result construction (especially Next field, Status value, Patch structure)

## Max Steps Exceeded

**Symptom**: Error message: "graph: max steps exceeded" and run aborts after 60 (or configured) node executions.

**Cause**: The run performed too many node executions without reaching END. Usually, the fix loop is looping forever.

**Diagnosis**:
1. Look at the journal to see which node ran last and what it returned
2. If the pattern shows fix → test → fix → test repeating, the agent cannot make progress

**Remedy**:
The run has ended, and `belay resume` will not continue it. Change belay.yaml or the goal, then start a new run:
- Increase graph.max_steps in belay.yaml (default 60)
- Or reduce the goal so fewer fixes are needed
- Or break the work into smaller runs (smaller goals are easier to fix)
- If fixes are not making progress, the issue may be fundamentally hard and require manual intervention

## Secrets in Logs

**Symptom**: Sensitive data (API keys, passwords, tokens) appears in journal or logs.

**Expected Behavior**: Belay redacts secrets on capture (see internal/exec/redact.go).

**Diagnosis**:
1. Belay redacts exact matches of environment variable values (specified in exec.BaseEnvNames and adapter-specific env lists)
2. If a secret is transformed (base64-encoded, URL-encoded, etc.) before printing, the redaction does not catch it
3. Check if SONAR_TOKEN, API_KEY, or other secret-bearing variables were passed to child processes

**Remedy**:
- Ensure secret-bearing variables are in the allowed environment (see internal/linter/golangci.go golangciEnv)
- If you see unredacted secrets, report it as a security issue
- Rotate the exposed credentials immediately
- Redaction runs at roughly 20 MB/s; very large output may exceed MaxOutput and bypass scanning

## Configuration Error

**Symptom**: `belay run` fails immediately with `belay: that configuration cannot be used:` followed by one line per problem, such as `review.fail_on: "bogus" is not valid (want one of: blocker, critical, major, minor, info)`. A value of the wrong type names the file and line. An `agent.backend` belay does not have stops it with `belay will not start this run.` and `belay does not have a backend called "nope".` Nothing is created and nothing is spent.

**Cause**: belay.yaml has a syntax error or invalid value.

**Diagnosis**:
1. Run `belay --dry-run run "your goal"`. It loads and checks the configuration and reports every problem, without creating or spending anything. belay has no `config` command
2. Look for typos in field names, invalid enum values, or missing required fields

**Remedy**:
- Check docs/config.md for the schema
- Fix the YAML syntax or values and retry
- Common mistakes: wrong agent.backend name, fail_on not in [info, minor, major, critical, blocker], budget.max_usd as a string instead of a number

## A Run Paused at `promote`

**Symptom**: The run stops with `belay paused ... and is waiting for you.`, `where: promote`, and a note naming files that conflict between a fanout candidate's branch and the run's own.

**Cause**: belay merges a fanout winner into the run's branch with a real merge, and two lines of work changed the same lines. Only a person can say what to keep, so belay pauses rather than guessing or failing — a failed run cannot be resumed, and this one has already been paid for.

**Diagnosis**:
1. The note names the conflicting files, the candidate's branch and the run's worktree
2. `belay timeline <run-id>` shows the same note with the rest of the run

**Remedy**:
- `cd` to the run's worktree (the note names it), run `git merge <candidate-branch>`, resolve the conflicts and commit
- Then `belay resume <run-id>`. promote asks the repository what is already merged, finds your merge, and finishes
- If you stop part way — conflicts resolved, nothing committed — resuming pauses again and says so. `git status`, `git commit`, then resume

## A Run Paused Because Its Worktree Has Uncommitted Changes

**Symptom**: A branch run pauses at `promote` with a note saying the branch "has changes belay did not commit".

**Cause**: Something changed files in the run's worktree that belay did not make, usually a merge finished part way by hand. belay will not sweep them into its own commit.

**Remedy**: `cd` to the run's worktree, `git status` to see what is uncommitted, `git commit` what belongs, then `belay resume <run-id>`.

## `belay resume` Says the Run's Worktree Is Gone

**Symptom**: `belay resume` refuses with `this run works in its own git worktree at ..., and that worktree is gone.`

**Cause**: The directory was deleted, or the whole `.belay/runs/<run-id>` directory was removed. Everything the run committed is still on its branch; only the tree it was working in is missing.

**Remedy**:
- Recreate it with the command the refusal prints: `git -C <repo> worktree add <path> belay/<run-id>`, then resume
- Or take what you need from the branch and start a fresh run: `git log -p <base>..belay/<run-id>`
- If the worktree is there but on another branch, put it back with `git -C <worktree> switch belay/<run-id>`

## belay Is Editing the Checkout Instead of Working on a Branch

**Symptom**: `belay run` says `belay is editing this workspace directly: ...` and your files change during the run.

**Cause**: `output.mode: auto` chose in-place editing. The reason is printed with it, and there are four: the directory is not inside a git repository, the repository has no commits yet, it has submodules, or git is older than 2.38.

**Diagnosis**: `belay --dry-run run "your goal"` prints the same reason under "where its result would go", without creating or spending anything.

**Remedy**:
- No commits: make one. `git add -A && git commit -m "initial"`
- git too old: `git --version`, then upgrade with `brew install git` or from https://git-scm.com/downloads. belay needs 2.38 for `git merge-tree --write-tree`, which is how it tells whether a merge conflicts without touching a working tree
- Submodules: not supported for branch output. A new worktree does not populate them, so the run's tests would fail for a reason unrelated to the change
- Not a git repository: `git init` and make a commit, or accept in-place editing
- Set `output.mode: "branch"` to make belay refuse rather than fall back

## `belay run --pr` Could Not Open a Pull Request

**Symptom**: The run finishes and reports its branch, and stderr says belay could not open the pull request. The exit status is still 0.

**Cause**: The change is committed on the branch either way, so this never fails the run. gh may not be signed in, the remote may not be a GitHub repository, or the push may have been rejected.

**Diagnosis**:
1. `gh auth status`
2. `git remote -v` — belay pushes to `branch.<name>.remote`, then `remote.pushDefault`, then the only remote, then `origin`
3. belay refuses `--pr` before the run starts when gh is missing or there is no remote, so a failure here happened at the push or at gh

**Remedy**: The closing report prints the two commands to finish by hand: `git push <remote> belay/<run-id>` and `gh pr create --draft --head belay/<run-id>`.

## Leftover `belay/*` Branches or Worktrees

**Symptom**: `git branch --list 'belay/*'` or `git worktree list` shows branches and worktrees from runs that are over.

**Cause**: They are kept on purpose, and sometimes by accident. A branch is a run's result and is never deleted by belay. A worktree is removed only when its run completed: a paused or failed run's is where you finish, so it stays. A process killed between creating the branch and writing the run's manifest leaves a branch no run names.

**Diagnosis**:
- `belay runs` lists the runs that exist. A `belay/<run-id>` branch whose id is not listed is an orphan
- `git worktree list` shows the worktrees; belay's are under `.belay/runs/<run-id>/worktree`

**Remedy**:
- Remove one worktree: `git worktree remove <path>`. Never `git worktree prune`, which also drops your own stale entries, such as worktrees on a disk that is not mounted
- Remove a branch once you are sure it holds nothing you want: `git log <base>..belay/<run-id>` first, then `git branch -D belay/<run-id>`
- Remove a whole finished run: `git worktree remove .belay/runs/<run-id>/worktree`, then `rm -rf .belay/runs/<run-id>`

<!-- slice:output -->

## The Plan Did Not Move Into the Repository

**Symptom**: A run finished, but its plan is still at `.belay/runs/<run-id>/artifacts/plan.md` and there is nothing in `docs/plans/`. belay said so on stderr, starting "the plan stays in the run directory".

**Cause**: belay moves a finished run's plan into the repository (`plan.archive`), and refuses rather than force it. The message says which of these it hit:

- `there is already a different file at ...` — something else is at that path, and belay never replaces a file somebody wrote
- `the way to ... leads outside the workspace` — the archive directory, or a directory on the way to it, is a symbolic link leading out of the repository
- `... changed after the run last read it` — `artifacts/plan.md` was edited after the run last read it, so it is no longer the plan the code was written from
- `the run's commit does not have the plan at ...` — a run on a branch whose `promote` node could not write the plan before committing; `--verbose` shows the reason it logged
- `the run has not finished successfully` — only a completed run's plan moves; a failed or stopped run's plan describes code that does not exist

A run stopped between finishing and moving its plan leaves it in the run directory too, with nothing said, because belay never got that far.

**Diagnosis**:
- `belay plan <run-id>` shows the plan wherever it is now, and says where that is
- `belay timeline <run-id>` is unaffected either way: the journal, the manifest and `state.json` never move

**Remedy**:
- Clear whatever the message named — move the file that is in the way, point `plan.archive_dir` inside the workspace — then run `belay plan --archive <run-id>`. Repeating it is harmless: belay adopts a copy it already placed rather than filing a second one
- Copy the plan across by hand if you would rather; nothing reads it again
- Set `plan.archive: false` to stop belay filing plans at all

## Git Ignores Everything in .belay, Including the Cassette

**Symptom**: `git add .belay/cassette.json` says the path is ignored.

**Cause**: The first run in a repository writes `.belay/.gitignore` holding `*`, so run state — journals, manifests, worktrees, agent transcripts — is never offered for a commit by accident. The recording `agent.mode: record` saves lives under `.belay` by default, so it is ignored with the rest.

**Remedy**:
- Record it somewhere else: `belay run --cassette ./testdata/cassette.json ...`
- Or commit that one file with `git add -f .belay/cassette.json`
- A `.belay/.gitignore` you wrote yourself is never replaced, so you can also write your own rules there

<!-- slice:plan -->

## belay fork Says Your Files Have Changed Since the Step Ran

**Symptom**: `belay fork <run-id> <node>` refuses, saying the files in your workspace "have changed since <node> #N ran", that belay "recorded no fingerprint" of them, or that they "were fingerprinted by a different belay".

**Cause**: A fork re-runs a step from the state the run was in just before it, and belay keeps that state, but not a copy of your files as they were then: only a fingerprint of them, taken before every step. If the files are not the ones the step saw, re-running it is a different experiment, so belay asks first. A run that edits your workspace in place changes it as it goes, so this is the usual answer for any step before the run's last change; so is a finished run's plan, filed into `docs/plans`. A run on a branch worked in its own worktree, which is removed once the run completes, and then only your checkout is left to copy.

**Remedy**:
- `belay fork <run-id> <node> --current-files` re-runs the step on the files as they are now, and records that it did
- Or fork a later step: the last one, after the run's last change to the files, usually needs nothing

## belay fork Says There Is No Snapshot of That Step

**Symptom**: `belay fork` refuses, saying a run "kept no snapshot" of the step, or that a step "was not run by" the run you named.

**Cause**: A fork starts from the copy of the run's state that belay saves before every step. A run recorded by a belay older than those snapshots has none, and state.json holds the state after the run, not before the step. A fork's own early steps were inherited, not run, and their snapshots are in the run it was forked from.

**Remedy**:
- For an inherited step, run the command the message gives, which forks the original run at the same step and number
- For an old run, start a new one

## belay resume Cannot Find a Fork

**Symptom**: `belay resume <fork-id>` answers "belay has no run called <fork-id>".

**Cause**: A fork's run lives inside its own copy of the repository, under `.belay/forks/<fork-id>`, not among the runs of the repository it was forked from.

**Remedy**:
- Point belay at the copy: `belay --workspace .belay/forks/<fork-id> resume <fork-id>`. `belay runs` in the original repository lists every fork with that command
- `rm -rf .belay/forks/<fork-id>` deletes a fork you no longer want; the run it was forked from never refers to it

<!-- slice:fork -->

## Another `belay queue start` Is Already Working Through This Queue

**Symptom**: `belay queue start` refuses with "another belay queue start is already working through this queue", naming a process id.

**Cause**: One start works through a queue at a time, across every terminal on the machine, so two never run two goals at once. The lock is held by the process for as long as it runs, and the operating system lets go of it the moment that process ends, however it ends.

**Remedy**:
- `belay queue status` says whether a start is working now, and on which goal
- Let it finish, or press Ctrl-C once in its terminal: the goal it is on pauses where it stands, and the next start carries it on
- A process id that no longer exists cannot hold the lock. If the refusal persists after that process is gone, the workspace may be on a network filesystem, where the lock is not reliable and the queue is not supported

## A Queued Goal Is Waiting for Plan Approval

**Symptom**: `belay queue` lists a goal as "waiting for plan approval", and `belay queue start` passes over it.

**Cause**: The goal's plan was written, and it stopped for you to read it, as every run does with `graph.approval` on. The queue parked it and moved on rather than wait for you.

**Remedy**:
- `belay queue` prints where the plan is. Read it, and edit it if you want to: belay reads the file again when it carries the goal on
- `belay queue approve <goal>`, then `belay queue start`. Approved goals run before the goals still waiting their turn
- To let a goal skip review, add it with `belay queue add --no-approval`. A goal already added keeps the choice made when it was added

## A Queued Goal Stopped at a Limit

**Symptom**: `belay queue` lists a goal as "paused at a limit", naming `budget.max_usd`, `graph.max_steps` or `graph.node_timeout`.

**Cause**: The goal's run met one of belay's own limits and paused. The queue never retries such a goal by itself: whether the limit was wrong is for you to decide.

**Remedy**:
- The goal's run reads the `belay.yaml` in its copy. Raise the limit there (`belay queue` prints the path), or pass the new value with `--config`
- Carry the goal on yourself with the command `belay queue` prints: `belay --workspace <copy> resume <run-id>`. Its run keeps everything it has already spent

## Where Is a Finished Goal's Change?

**Symptom**: `belay queue` says a goal finished, and your checkout has not changed.

**Cause**: That is by design. Each queued goal runs in its own copy of the workspace, under `.belay/queue/work/<run-id>`, and belay never edits your checkout from the queue.

**Remedy**:
- `belay --workspace <copy> timeline <run-id>` shows what the run did and which files it changed
- Take what you want from the copy by hand, for example with `diff -ru <file> <copy>/<file>` for each changed file. The copy has no `.git`, so it is compared, not merged
- Delete the copy once you have what you need: `rm -rf .belay/queue/work/<run-id>`. Nothing removes copies for you

## A Queued Goal's Tests Fail on Something Missing

**Symptom**: A goal fails at its tests with an error about a missing dependency, while the same tests pass in your checkout.

**Cause**: A goal's copy is made like a fanout candidate's, without `.git`, `node_modules`, `vendor`, `.venv`, `__pycache__`, `dist`, `build`, `target` and `.belay`. Anything your tests need from those is not in the copy.

**Remedy**:
- Make the test command rebuild what it needs, for example with `test.runner: custom` and a script that installs dependencies first
- Commit what the tests need, if it belongs in the repository

## The Last Start Was Cut Off

**Symptom**: `belay queue status` says the last start "was cut off before it could record stopping".

**Cause**: The process ended without the chance to say so: the machine rebooted or lost power, or the process was killed. Nothing is lost. The queue records each goal's claim before it makes anything for it, so it knows exactly what the start had in hand.

**Remedy**:
- `belay queue start`. It carries on with the goal that was in hand, in the run it was already in, re-running only the step that was cut short, and then the rest

## belay Refuses to Read the Queue

**Symptom**: Every `belay queue` command fails with "queue: corrupt", naming a seq or a line.

**Cause**: `.belay/queue/queue.ndjson` holds a history no belay writes: a line damaged in the middle of the file, or a record out of order. belay refuses rather than guess, because a wrong guess about whether a goal was already claimed is how a goal would run twice. A damaged last line alone is not this: belay discards it as an interrupted write.

**Remedy**:
- Move the file aside (`mv .belay/queue/queue.ndjson .belay/queue/queue.ndjson.broken`) and add the goals you still want again. The runs the old queue made are untouched, in `.belay/queue/work/`
- Look at the line the error names first: it says which goal and which change

<!-- slice:queue -->

## A Run Stopped on a Policy Rule

**Symptom**: A run ended at the `policy` step, with a note naming a rule such as `policy:forbidden_path` or `policy:max_changed_files`.

**Cause**: `belay.yaml` declares guardrails the change must obey whatever its tests say, and the change broke one belay will not ask an agent to repair. `forbidden_paths`, `max_changed_files` and `max_changed_lines` end the run: a change that touched a path it may not touch is not something to send back for another attempt. The other rules go to the fix loop instead.

**Diagnosis**:
- `belay timeline <run-id>` shows the `policy` step and the rules that fired
- The full report is at `.belay/runs/<run-id>/artifacts/policy-<step>.json`, listing every violation with its file and, for a content match, its line
- `.belay/runs/<run-id>/artifacts/diff-<step>.patch` is the change that was judged

**Remedy**:
- If the change should not have touched what it touched, the run did its job: narrow the goal and start a fresh run
- If the rule is wrong, change it in `belay.yaml` and start a fresh run. `belay resume` will **not** apply the new rule: belay holds a run to the rules it started with, which is why the gate can trust the baseline it recorded then
- `belay run --dry-run` prints every rule and says which ones end a run, so the next run holds no surprises

## A Policy Was Added and the Run Ignores It

**Symptom**: `belay.yaml` declares a policy, but `belay resume <run-id>` says the run keeps going without one, and no `policy` step appears in its route.

**Cause**: This is deliberate, and belay warns about it. The rules and a survey of what the workspace declared are recorded in the run's `manifest.json` when the run is created. A run that started before the policy existed never took that survey, so there is nothing its change could honestly be judged against — every dependency in the tree would look new, and the run would be failed for work it did not do.

**Remedy**:
- Start a fresh run; it picks the policy up.
- The reverse holds too: a run started **with** a policy keeps it even if you delete the rules before resuming, and belay warns about that as well.


<!-- slice:backend -->
