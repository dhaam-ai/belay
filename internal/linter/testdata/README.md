# internal/linter testdata

Every file here is **hand-authored**. None was captured by piping a real
linter's output into a file, and no test in this package invokes a real
linter — the adapters reach a subprocess only through the injectable
`CommandRunner` seam, and `TestNoRealLinterInvocation` fails the build if a
test file bypasses it.

The fixtures are hand-written *against verified schemas*, not from memory:

| Tool | Schema source |
| --- | --- |
| golangci-lint | `pkg/printers/json.go` (`JSONResult{Issues, Report}`) and `pkg/result/issue.go` (`FromLinter`, `Text`, `Severity`, `Pos` as a `go/token.Position`, all untagged, hence the capitalized wire keys), cross-checked against a real `golangci-lint v2.12.2 run --output.json.path=stdout` on a throwaway module outside this repository. |
| ESLint | <https://eslint.org/docs/latest/integrate/nodejs-api> — `LintResult` and `LintMessage`: *"The severity of this message. 1 means warning and 2 means error"*, `ruleId` *"is null"* for core messages, `fatal` is *"true if this is a fatal error unrelated to a rule, like a parsing error"*. |
| ruff | `crates/ruff_db/src/diagnostic/render/json.rs` — the `JsonDiagnostic` struct and its own snapshot test, which pins the key set and shows `"severity": "error"` hardcoded outside preview mode. |

## Files

| File | What it is for |
| --- | --- |
| `golangci-issues.json` | A spread across all five severities: `typecheck` (blocker), `gosec` (critical), `errcheck` and `govet` (major), `revive` (minor, carrying its own `"Severity":"warning"`), `misspell` (info), plus two linters the mapping table has never heard of — one with a severity string, one without — to exercise both fallbacks. |
| `golangci-clean.json` | A run that found nothing: `"Issues": []`, exit 0. |
| `golangci-stdout-with-summary.txt` | Not a `.json` file on purpose. It reproduces what v2.12.2 actually writes to **stdout**: the JSON document, and then a human-readable `N issues:` tally on the same stream. Decoding all of stdout as one document fails on every run that finds something, so this fixture guards the decoder. |
| `eslint-issues.json` | Both documented severities (2 error, 1 warning), a `fatal` parsing error with a null `ruleId`, a non-fatal message with a null `ruleId`, and a `suppressedMessages` entry that must **not** be counted. |
| `eslint-clean.json` | A file that was linted and had nothing wrong with it — the realistic clean shape, which is a non-empty array of results with empty `messages`. |
| `ruff-issues.json` | `E999` (blocker), `F821` and `S105` (critical), `B006` (major), the `S101` and `F401` carve-outs plus `E501` (minor), `I001` (info), and an unknown `ZZZ999` for the default. Every entry carries ruff's hardcoded `"severity": "error"`, which the mapping must ignore. |
| `ruff-clean.json` | `[]`, ruff's clean output. |
| `malformed.json` | A JSON body truncated mid-object, shared by all three adapters. It must produce a typed `*OutputError` and a `GateError` report — never a panic, and never a `GateFail` that would send an agent to fix findings that were never parsed. |
