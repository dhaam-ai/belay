# Troubleshooting

This document covers common failure modes, their symptoms, and remedies.

## The Agent Edited Nothing

**Symptom**: The run completed but no files changed, or the generated code is empty.

**Possible Causes**:
- The prompt was too vague or the goal was unachievable
- The agent backend did not understand the task (check SystemPrompt in the config)
- The working directory was wrong (verify WorkDir in logs)
- The agent ran out of budget mid-task

**Diagnosis**:
1. Run `belay timeline <run-id>` to inspect the journal
2. Look for node_finished records with Status="ok" but empty output
3. Check the agent's Raw response field (if present in logs) for error messages
4. Verify the goal in manifest.json is what you intended

**Remedy**:
- If the goal is unclear, edit belay.yaml to provide more context or a better prompt
- If the agent ran out of budget, increase review.budget_limit_usd
- If the working directory or model is wrong, edit belay.yaml and retry with `belay resume`

## Run is Paused

**Symptom**: `belay run` says the run is paused and refuses to continue.

**Cause**: The approve node paused the run awaiting human approval.

**Diagnosis**:
1. Run `belay timeline <run-id>` and look for the journal entry with Event="run_paused"
2. The approve node's Note field may contain a URL or instructions

**Remedy**:
1. Review the proposed plan (in the manifest or logs)
2. If approved, run `belay resume <run-id>` to continue
3. If not approved, `belay run` the same config again to start fresh (runs have monotonic IDs; old runs are not re-executed)

## Test Failed; Agent Cannot Fix It

**Symptom**: The test node found failing tests, the agent tried to fix them with `fix` node, but the fixes failed the gate or produced empty output.

**Possible Causes**:
- The test failure message was unclear or incomplete
- The agent does not have enough turns to fix the issue
- The fix loop hit its max retry limit (graph.give_up)
- The problem requires external setup (a database, a service) the agent cannot reach

**Diagnosis**:
1. Look at TestReport.Failures in the journal to see what tests failed
2. Check agent invocation prompts in logs (the Prompt and SystemPrompt fields)
3. Count how many times the fix node ran (look for attempt numbers in node_finished records)
4. Check if the run hit the retry limit (ErrMaxSteps, usually 50 node executions)

**Remedy**:
- If tests require external services, set them up and retry
- If the agent needs more context, augment the goal or SystemPrompt in belay.yaml
- If the loop is infinite, increase graph.give_up (default 10) to allow more fix attempts
- If the limit is reached, break the loop by reducing the goal to a smaller, more achievable task

## Toolchain Missing

**Symptom**: Error message: "belay: required toolchain is not installed" and the run aborts immediately.

**Cause**: A required binary (go, pytest, golangci-lint, sonar-scanner, etc.) is not on PATH.

**Diagnosis**:
1. The error message names which toolchain is missing (e.g., "golangci-lint")
2. Run the same command in a shell to see what the error is: `which <tool>`

**Remedy**:
- Install the toolchain and retry with `belay resume`
- Or, if using a different test runner or linter, update belay.yaml (runner.name, review.mode)

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

**Symptom**: Run aborts with error: "belay: budget exceeded: spent $X of $Y limit"

**Cause**: Cumulative cost of all agent invocations crossed the configured ceiling.

**Diagnosis**:
1. Check review.budget_limit_usd in belay.yaml
2. Look at Usage.USD fields in journal records to see which invocations cost the most
3. Check if Usage.Estimated = true; estimated costs are lower-confidence

**Remedy**:
- Increase review.budget_limit_usd in belay.yaml (if the project is worth it)
- Use a cheaper model (adjust agent.model in belay.yaml)
- Scope the goal to fewer changes (smaller diffs cost fewer tokens)
- If costs are overestimated, they may drop in a future API pricing update

## Crashed Run

**Symptom**: `belay run` was interrupted (killed, network down, process exited) and a run directory exists at `.belay/runs/<run-id>/`.

**Diagnosis**:
1. Run `belay timeline <run-id>` to see how far the run got
2. Look at the journal's final record: if it ends in run_paused or run_completed, the run is finished; if it ends in node_finished, the run can resume

**Remedy**:
1. Run `belay resume <run-id>` to continue from where it stopped
2. The journal file is automatically healed on resume (any torn tail is discarded)
3. If the same crash happens repeatedly, it may indicate a stuck node (increase graph.node_timeout in belay.yaml)

## Fanout Refused

**Symptom**: Error message: "graph: fanout disabled" or "join is not implemented"

**Cause**: Fanout (best-of-N candidates) is disabled in this version.

**Diagnosis**:
- Check config.Fanout.Enabled in belay.yaml (should be false)
- Or the plan node returned Next="fanout" but fanout is disabled

**Remedy**:
- Fanout is currently disabled (see ADR-0004); do not configure code nodes to return Next="fanout"
- In a future version when join is implemented, set config.Fanout.Enabled = true in belay.yaml

## SonarQube Connection Failed

**Symptom**: Review node aborts with error: "sonar: could not connect" or "sonar: authentication failed"

**Cause**: 
- SonarQube server is unreachable (wrong URL, network down, firewall)
- Authentication token is missing or incorrect
- Docker is not installed (in Docker mode)

**Diagnosis**:
1. Check review.mode in belay.yaml (must be "sonar-server" or "sonar-docker")
2. For server mode, verify SONAR_TOKEN is set and the server URL is correct
3. For Docker mode, verify `docker` is on PATH and the daemon is running
4. Try connecting manually: `curl -H "Authorization: Bearer $SONAR_TOKEN" $SONAR_URL/api/system/status`

**Remedy**:
- Server mode: Set SONAR_TOKEN environment variable and verify the server URL
- Docker mode: Install Docker or switch to server mode
- If the server is temporarily down, `belay resume` will retry the review node
- If authentication fails, rotate your token and retry

## Quality Gate Failed

**Symptom**: Review node returns a report with Gate="fail"; run branches to the fix node.

**Expected Behavior**: This is not an error; it is the design. The run tries to fix the issues automatically.

**Diagnosis**:
1. Look at QualityReport.Issues in the journal to see what problems were found
2. Check the Severity of each issue and compare against review.fail_on
3. Look at the linter or reviewer's Name to see which tool produced the report

**Remedy**:
- If the fixes are working, wait for the fix loop to complete
- If the loop is stuck (same failures after many attempts), the issue may be unfixable by code edits alone (e.g., a missing type import from a broken dependency)
- If the gate threshold is wrong, edit review.fail_on in belay.yaml and retry
- If the underlying tool (golangci-lint, sonar-scanner) is misconfigured, update .golangci.yml or sonarqube configuration and retry

## Quality Gate Fails on Code the Run Did Not Touch

**Symptom**: With `review.mode: lint`, every run fails the gate whatever it changed, and the fix node asks the agent to repair findings in files the run never edited.

**Cause**: The gate is counting findings that were already in the repository. From 0.1.2, golangci-lint reports only findings on lines that differ from the last commit. That needs `git` on `PATH` and a commit that holds the workspace. From 0.1.3, belay checks for both first. If either is missing, every finding counts. belay then logs `cannot scope golangci-lint to the run's change; every finding counts` with the reason, and the gate's summary ends `not scoped to the run's change`. This happens outside a repository, in a repository with no commits, and in a directory the repository ignores or hasn't committed yet. belay 0.1.1 and earlier always counted every finding, and ESLint and Ruff still do.

**Diagnosis**:
1. Run `belay version`. On 0.1.1 or earlier, every existing finding counts.
2. Look for the warning above in the output of `belay run --verbose`. Or look for `not scoped to the run's change` in the run's review report, `.belay/runs/<run-id>/artifacts/review-<step>.json`.
3. In the workspace, run the check belay makes:
   ```bash
   git ls-tree --name-only HEAD
   ```
   If it fails or prints nothing, belay can't scope the findings.
4. Reproduce what the gate sees:
   ```bash
   golangci-lint run --new=false --new-from-rev=HEAD --new-from-merge-base= --new-from-patch= --whole-files=false ./...
   ```

**Remedy**:
- Upgrade to belay 0.1.3 or later. 0.1.2 scopes findings, but lets a repository's `.golangci.yml` `issues` settings change the scope
- Run belay in a git repository with `git` on `PATH`, and commit the workspace first. If the workspace is in a directory the repository ignores, run belay from a repository that tracks it
- Commit or stash unrelated edits before the run: uncommitted changes count as part of the run's change
- If a finding comes from `typecheck`, fix the build first. A package that does not compile fails the gate whether or not the run changed it
- For ESLint and Ruff, fix or exclude the existing findings in the tool's own configuration, or raise `review.fail_on`, for example to `critical`

## Quality Gate Passes Although golangci-lint Reports Findings

**Symptom**: `golangci-lint run ./...` reports findings in the workspace, but the run's lint gate passed.

**Cause**: From 0.1.2, the gate counts only findings on lines that differ from HEAD. That leaves out findings committed before the run, which is deliberate, and four kinds it misses:
- A finding the change causes on a line it didn't touch, such as an unchecked error where a function that now returns one is called. Compile errors are the exception: they always count
- A finding in a file git ignores
- A change committed while the run was in progress
- A finding golangci-lint's cache took from another checkout holding identical code. The cache stores the other checkout's paths, and the scoping drops them ([golangci/golangci-lint#3502](https://github.com/golangci/golangci-lint/issues/3502))

**Diagnosis**:
1. Run the command from step 4 in the section above, and compare its findings with `golangci-lint run ./...`
2. Check whether the finding's file is ignored: `git check-ignore -v <file>`
3. Check for commits made since the run started: `git log`

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
- Increase graph.node_timeout in belay.yaml
- Or reduce the scope (fewer tests, smaller code changes, scoped SonarQube analysis)
- If timeouts happen repeatedly on the same node, there may be an infinite loop or a genuine performance problem

## Invalid Result

**Symptom**: Error message: "invalid result: ..." with details about what failed validation.

**Cause**: A node returned a Result that the dispatcher could not process.

**Expected Behavior**: This is a bug in the node implementation, not a user error.

**Diagnosis**:
1. Look at the error message; it usually specifies what field was invalid (e.g., "unknown next node", "invalid status", "nil patch")
2. This typically happens only during development or if belay itself has a bug

**Remedy**:
- Report the error to the belay team with the full journal
- If you wrote a custom node, review your Result construction (especially Next field, Status value, Patch structure)

## Max Steps Exceeded

**Symptom**: Error message: "graph: max steps exceeded" and run aborts after 50 (or configured) node executions.

**Cause**: The run performed too many node executions without reaching END. Usually, the fix loop is looping forever.

**Diagnosis**:
1. Look at the journal to see which node ran last and what it returned
2. If the pattern shows fix → test → fix → test repeating, the agent cannot make progress

**Remedy**:
- Increase graph.max_steps in belay.yaml (default 50)
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

**Symptom**: `belay run` fails immediately with error like "config: unknown backend" or "config: invalid fail_on severity".

**Cause**: belay.yaml has a syntax error or invalid value.

**Diagnosis**:
1. Run `belay config validate belay.yaml` (or similar command; check `belay --help`)
2. Look for typos in field names, invalid enum values, or missing required fields

**Remedy**:
- Check docs/config.md for the schema
- Fix the YAML syntax or values and retry
- Common mistakes: wrong agent.backend name, fail_on not in [info, minor, major, critical, blocker], budget_limit_usd as a string instead of a number
