# internal/sonar/scanner testdata

**Every file here is hand-authored.** None was captured by running a real
scanner, because there is no SonarQube server and no Docker daemon in this
project's development or CI environment, and ADR 0008 promises contributors can
run the whole suite without either. No test in this package starts a process:
the scanner is reached only through the injectable `Execer` seam, `TestMain`
empties `PATH` for the test binary so an accidental real invocation cannot
resolve `docker`, and `TestNoRealSubprocessInEnabledTests` fails the build if a
default-tagged test file constructs a real `exec.Runner`.

Hand-authored is not the same as invented. Each fixture reproduces message
shapes verified against primary sources:

| Line | Source |
| --- | --- |
| `QUALITY GATE STATUS: FAILED - View details on <url>`, preceded by `INFO: EXECUTION FAILURE` and `ERROR: Error during SonarScanner execution` | A scanner console log posted verbatim in <https://community.sonarsource.com/t/quality-gate-status-failed-view-details-on-masked-dashboard-id-house/42164>, which also pins `INFO: ------------- Check Quality Gate status` and `INFO: Waiting for the analysis report to be processed (max 300s)`. |
| `max 300s` in the wait line | `sonar.qualitygate.timeout` defaults to 300 seconds — SonarQube Server docs, *Analyzing source code › CI integration › Overview*. |
| `ERROR: Quality Gate check timeout exceeded` | <https://community.sonarsource.com/t/scans-failing-with-error-quality-gate-check-timeout-exceeded/86189>. |
| `INFO: Base dir: /usr/src` and `INFO: Working dir: /tmp/.scannerwork` | The image's own `Dockerfile` (`SRC_PATH=/usr/src`, `SCANNER_WORKDIR_PATH=/tmp/.scannerwork`, `WORKDIR ${SRC_PATH}`) and `bin/entrypoint.sh`, which passes `SCANNER_WORKDIR_PATH` through as `-Dsonar.working.directory`. <https://github.com/SonarSource/sonar-scanner-cli-docker> |
| `docker: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?` and the `Unable to find image ... locally` / `Error response from daemon: manifest unknown` pair | Docker CLI output, with the accompanying exit statuses taken from Docker's documented rule: "Exit code `125` indicates that the error is with Docker daemon itself" and "Any exit code other than `125`, `126`, and `127` represent the exit code of the provided container command." <https://docs.docker.com/engine/containers/run/> |
| `ERROR: Not authorized. Analyzing this project requires authentication...` | The message family SonarQube returns for a rejected token. **Partially verified**: the wording was reconstructed from community reports rather than quoted from a doc page, so it is the least authoritative fixture here. Nothing depends on its exact words — the parser reaches "no verdict" for it by finding no `QUALITY GATE STATUS:` line at all, which is why an auth failure is `GateError` regardless of how the message is phrased. |
| `ERROR: SonarQube server [<url>] can not be reached` | Same caveat, same reason: it is classified by the absence of a verdict, not by its text. |

## Files

| File | Stream | Exit | What it pins |
| --- | --- | --- | --- |
| `gate-pass.txt` | stdout | 0 | The passing verdict, `QUALITY GATE STATUS: PASSED`, under an `INFO:` prefix. `GatePass`, nil error. |
| `gate-fail.txt` | stdout | 1 | **The crux.** The failing verdict arrives with *no* level prefix, folded into the execution-failure block, and the process exits non-zero. Must produce `GateFail` and a **nil error**, because a failed gate routes to the fix node like a failing test. This is also the "issue counts" fixture: the report it produces carries `Counts{Blocker: 1}` from the one synthesized gate issue — the scanner CLI prints no per-issue detail, so a fixture claiming otherwise would be a fabrication. |
| `gate-wait-timeout.txt` | stdout | 1 | The server had not finished computing the gate. Must be `GateError` wrapping `ErrGateWaitTimeout`, **never** `GateFail` — belay never learned the answer. |
| `no-verdict.txt` | stdout | 0 | `sonar.qualitygate.wait=false`: analysis succeeded, no verdict printed. Exit zero must **not** be read as a pass; it is `GateError` wrapping `ErrNoGateVerdict`. |
| `auth-failure.txt` + `auth-failure.stderr.txt` | stdout + stderr | 1 | The only split fixture. It exists to prove the parser reads stdout and stderr together, since the scanner's level routing is not something belay controls. `GateError`. |
| `unreachable-host.txt` | stdout | 1 | Nothing was analyzed. `GateError`. |
| `docker-image-missing.txt` | stderr | 125 | `docker run` could not obtain the image. Must translate to `*belay.ToolchainError` so `errors.Is(err, belay.ErrToolchainMissing)` holds and the graph routes to "a human installs software". |
| `docker-daemon-down.txt` | stderr | 1 | A daemon that is not running exits **1**, which the exit-status rule would otherwise read as the container's own status. Matched by message instead. Also a `*belay.ToolchainError`. |
