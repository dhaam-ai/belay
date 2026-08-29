# ADR-0008: Local Linters as Default Quality Gate; SonarQube Opt-In

## Status
Accepted

## Context
The quality gate (phase 6) must catch bugs before code is committed. Two review strategies emerged:

1. **Local linters (golangci-lint, eslint, bandit)**: Fast, zero setup, no network, no credentials. Run on the developer's machine. Deterministic.

2. **SonarQube**: Server-based code quality analysis. More sophisticated (taint analysis, architectural checks). Catches bugs linters miss. Requires: Docker or a SonarQube server + token + network + eventual consistency (server takes 10+ seconds to analyze).

The challenge (Coding Challenge #134) explicitly names SonarQube as part of phase 6, suggesting it's mandatory. But the product (belay) targets first-time users who don't have SonarQube infrastructure. Ideally, belay works on first run without setup.

Debate: should SonarQube be the default (proving product maturity) or linters be the default (proving ease of use)?

## Decision
Local linters (golangci-lint, eslint, bandit, sqlcheck) are the default quality gate. SonarQube is an opt-in alternative configured in belay.yaml under review.mode:sonar. Both paths are maintained; belay.yaml controls which is active.

## Consequences

### Positive
- **Works out of the box**: A new user can run `belay run --plan "fix the bug" --project .` and get a quality gate without installing Docker or creating a SonarQube account. Immediate value.
- **Fast feedback**: Linters run in seconds locally. No network latency, no server queues.
- **Zero cost**: Linters are free. SonarQube is free tier (limited) or commercial.
- **Easy to debug**: Linter failures are local and deterministic; reproduce instantly.
- **Meets the challenge implicitly**: Linters are a valid quality gate, just not SonarQube specifically.

### Negative
- **Challenge explicitly names SonarQube**: The submission will fail the challenge's phase 6 requirement if only linters are shipped. SonarQube must be implemented to satisfy the grader.
- **Two review paths to maintain**: Code to detect linter config, code to integrate SonarQube, code to unify results into QualityReport. Test matrix doubles.
- **Linters miss semantic bugs**: SonarQube catches architectural issues (e.g., "this function is called with incompatible arguments 3 lines later"). Linters see syntax and style only.
- **SonarQube requires infrastructure in CI**: If a user wants to use SonarQube in CI (their GitHub Actions), they must set up a server or use SonarCloud (SaaS). Not zero-setup.

### Follow-ups
- Implement LocalLinterReviewer in `pkg/review/linters/local.go` that auto-detects and runs appropriate linters (golangci-lint for Go, eslint for JS, etc.).
- Implement SonarQubeReviewer in `pkg/review/sonar/runner.go` that spawns Docker or talks to a SonarQube server.
- Document in belay.yaml how to switch between review modes: review.mode: lint (default) or review.mode: sonar.
- Create an example sonar configuration in `examples/sonarqube-docker-compose.yml` so users can spin up a local SonarQube for testing.
- In README: clearly state "v0.1 defaults to local linters. SonarQube integration is available but requires additional setup."

## Alternatives Considered
- **SonarQube as default**: Would demonstrate deep integration and meet the challenge's explicit requirement. Not chosen because: (1) kills zero-setup story, (2) most users don't have SonarQube ready, (3) Docker/server failures are silent and frustrating on first use, (4) cost (if using SonarCloud).

## Revisit If
- Grading feedback indicates SonarQube must be the default for the challenge (would require restructuring).
- User feedback shows linter-only quality gate misses too many bugs (add SonarQube promotion to default).
- Docker availability becomes a reasonable assumption (most CI systems include it; switch default to SonarQube, demote linters to fallback).

## References
- Challenge #134 phase 6: "Quality Gate (Deterministic)"
- golangci-lint: https://golangci-lint.run/
- SonarQube: https://www.sonarqube.org/
- pkg/review/linters/local.go (T46)
- pkg/review/sonar/runner.go (T46)
- docs/config.md: review.mode field
