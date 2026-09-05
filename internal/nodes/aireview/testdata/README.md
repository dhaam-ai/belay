# testdata — hand-authored MCP frames

Every file in this directory was **hand-authored** for this package's tests.
None of it was captured from a live SonarQube MCP server, a SonarQube Cloud
account, or a `sonarsource/sonar-mcp-server` container: T32 forbids
contacting a server, running Docker, or making any network call, so there
was no live server available to record from.

They are JSON-RPC 2.0 response frames as an MCP server would send them, and
they are fed to a real `*mcp.Client` over an in-memory fake transport
(`fakeTransport` in `helper_test.go`). That means the tests exercise the
package's real decode path — `mcp.Client.CallTool`, `ToolResult.JSON`, the
`structuredContent`/text-block fallback, `SonarIssue.BelaySeverity`,
`mcp.RelativeFile`, `mcp.BelayGate` — rather than a mock of it.

Their shapes follow two sources:

- **Tool parameter and result field names** follow
  `internal/sonar/mcp/tools.go`, whose author documented them against
  <https://docs.sonarsource.com/sonarqube-mcp-server/reference/tools> and
  the corresponding SonarQube Web API response shapes
  (`api/qualitygates/project_status`, `api/issues/search`). That package's
  own doc comment records the same caveat: the response shapes are a stated
  assumption, not a verified capture.
- **The JSON-RPC envelope** follows the MCP `tools/call` result shape
  (`content`, `structuredContent`, `isError`) that `internal/sonar/mcp`
  implements.

Every `"id"` is the placeholder `0`; `withID` in `helper_test.go` rewrites
it to whatever id the client actually sent, so one fixture can answer a
request wherever it lands in a call sequence.

| File | What it is |
| --- | --- |
| `initialize_response.json` | A successful `initialize` handshake. |
| `tools_list_response.json` | A `tools/list` advertising both tools this package calls (plus unused ones). |
| `tools_list_missing_gate.json` | A `tools/list` that omits `get_project_quality_gate_status` — the "tool not advertised" deployment. |
| `gate_pass.json` | `get_project_quality_gate_status` → `"OK"`. |
| `gate_fail.json` | `get_project_quality_gate_status` → `"ERROR"`, with conditions. |
| `gate_none.json` | `get_project_quality_gate_status` → `"NONE"`: no analysis has run, so there is no verdict. |
| `issues_none.json` | `search_sonar_issues_in_projects` → zero issues. |
| `issues_mixed.json` | `search_sonar_issues_in_projects` → five issues spanning both severity vocabularies, a multi-impact issue, an unrecognized severity, and a line-less finding. |
| `server_error.json` | A JSON-RPC error object: the server refused the call. |
