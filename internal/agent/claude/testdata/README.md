# Fixtures

Every file here is **hand-authored**. None was recorded from a live `claude`
run: invoking the real CLI costs money, so the package is structured so that
its subprocess seam (`claude.Runner`) is injectable and every test feeds these
bytes through a fake instead.

They are written to match the documented shape of the object
`claude -p "<prompt>" --output-format json` prints — the `result` message
described in <https://code.claude.com/docs/en/headless> and typed as
`SDKResultMessage` in <https://code.claude.com/docs/en/agent-sdk/typescript>.
Field names and their meanings were taken from those pages and from
<https://code.claude.com/docs/en/agent-sdk/cost-tracking>. Token counts and
dollar figures are invented and are chosen to make the arithmetic in the tests
easy to check by hand; they are not real spend.

| File | Stands for |
|---|---|
| `success_with_cost.json` | A normal successful run that reports `total_cost_usd` and `modelUsage`. Exercises the exact-cost path (`Usage.Estimated == false`). |
| `success_without_cost.json` | A hypothetical future CLI that dropped the cost fields but kept `usage`. Exercises the price-table fallback (`Usage.Estimated == true`). |
| `success_unknown_fields.json` | The same success shape carrying fields this package has never heard of, at the top level, inside `usage`, and inside a `modelUsage` entry. Proves unknown fields are ignored rather than fatal, and that `AgentResponse.Raw` keeps the original bytes. |
| `malformed_truncated.json` | A JSON body cut off mid-string, as a killed or crashed CLI would leave it. Must produce a `*ParseError`, never a partial `AgentResponse`. |
| `signed_out_exit_1.json` | A CLI with no credential. It prints its explanation as the `result` of an `is_error` object on stdout, writes nothing to stderr, and exits 1. The shape follows what `claude` 2.1.274 printed in that state, trimmed and with an invented session id. Must produce an `*InvokeError` whose message is the CLI's own words. |
| `stderr_only_failure.txt` | The stderr of a CLI that rejected a flag before the run started, leaving stdout empty. Must produce a `*InvokeError`, and must not be confused with a missing toolchain. |
