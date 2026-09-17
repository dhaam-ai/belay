# Contributing to belay

Thank you for your interest in contributing to belay!

## Commit Messages

This project uses [Conventional Commits](https://www.conventionalcommits.org/) for all commit messages.

Each commit should follow this format:

```
<type>: <description>

<optional body explaining why, not what>
```

**Types:**
- `feat` — New feature
- `fix` — Bug fix
- `refactor` — Code change without behavior change
- `test` — Test additions or updates
- `docs` — Documentation only
- `chore` — Tooling, dependencies, config

**Example:**
```
feat: add version command to CLI

Displays semver, commit hash, and build date.
Injected via ldflags during build.
```

For more guidance, see the `.gitmessage` template in the repository root.

## Tests

`make test` runs the whole suite with the race detector. The tests in
`test/lintgate` run the real golangci-lint and git, and skip when either is
missing. `go test` only says why with `-v`:

```bash
go test -v ./test/lintgate
```

A skipped test's message says what to install.
