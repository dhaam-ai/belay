// Package write implements belay's write node: the step between code and
// test that turns "the agent says it changed these files" into a verified,
// recorded fact.
//
// # The design decision: verify and record, never apply
//
// This node does not modify the workspace. It reads it.
//
// That is the central judgement in this package, and it follows from what
// belay's only shipped agent backend actually does. Per ADR-0005, v0.1 ships
// Claude Code, and the adapter in internal/agent/claude runs the CLI with
// [belay.AgentRequest.WorkDir] as its working directory. Claude Code edits
// files in place. By the time the write node runs, the change is already
// materialized on disk; there is nothing left to apply. A node that applied a
// diff on top would double-apply the very edits the agent just made.
//
// Three properties fall out of that choice, and each is worth more than the
// generality an apply path would have bought:
//
//   - Re-running is safe by construction. ADR-0002 requires it: the
//     dispatcher re-executes a node whose node_started record has no matching
//     node_finished, because that is how a crash inside a node presents on
//     resume. A node that mutates nothing is idempotent without bookkeeping.
//     An apply-based node would need to persist "have I already applied
//     this?" somewhere — which is exactly the control state ADR-0002 forbids
//     a node from owning.
//
//   - A cancelled write cannot leave a half-applied tree, because there is no
//     partially-applied state to leave. Criterion met by design rather than
//     by careful cleanup code that only runs when it is reached.
//
//   - No toolchain dependency. The obvious alternative — shell out to git and
//     read `git diff` — is unavailable where belay needs it most:
//     internal/isolate/dircopy deliberately omits .git from a candidate
//     workspace ("omitting it means a candidate cannot run git commands,
//     which is the trade ADR-0004 already accepted"), so a git-based write
//     node would be broken for every best-of-N candidate. Nothing in this
//     package shells out, so internal/exec is not needed and
//     belay.ErrToolchainMissing can never arise.
//
// The cost of the choice is stated plainly: because belay keeps no pre-change
// baseline of file contents, this node cannot compute a minimal delta. The
// artifact it writes is a full materialization record — every changed file
// rendered as a unified diff against /dev/null — which is a truthful account
// of what is on disk rather than a delta it would have to invent. See
// [renderPatch].
//
// # Threat model
//
// Every path this node handles originates, ultimately, in model output. A
// change set is therefore treated as hostile input, not as a manifest:
//
//   - Containment is structural, and it is enforced twice by two independent
//     mechanisms that must agree. First lexically: a claimed path is refused
//     unless it can be proven to denote a regular file strictly inside the
//     workspace root, checked against the path as written and again after
//     symlink resolution, always by whole path segments so that "/a/bc" is
//     never mistaken for something inside "/a/b" (see [safeJoin]). Then in
//     the kernel: every stat and every read goes through an [os.Root] opened
//     on the workspace, which resolves each path inside that directory and
//     refuses any traversal that leaves it (see [openRoot]).
//
//     The second layer is not redundancy. The lexical gate decides at one
//     instant and the read happens at another, and in between the workspace
//     still belongs to an agent process that may be running: a file checked
//     as a regular file can become a symlink out of the tree before it is
//     opened. os.Root closes that window, because the check and the open are
//     the same syscall. The lexical gate is what the first layer is for —
//     refusing before any syscall, producing the typed refusal the run
//     reports, and enforcing rules the kernel knows nothing about, such as
//     .belay being off-limits.
//
//   - A refusal fails the node. It does not drop the offending entry and
//     carry on: a change set naming a path outside the workspace means
//     something upstream is wrong, and continuing would let the run proceed
//     on a record known to be false.
//
//   - Refusal happens before any read. The diff artifact is fed to the fix
//     and review nodes and archived in the run directory, so a symlink
//     pointing out of the tree is not merely a write hazard — followed, it
//     would quote a file the run was never allowed to see into a document
//     that outlives it.
//
// # What the node produces
//
// On success it routes to [graph.NodeTest] with a [state.Patch] carrying a
// [state.Code] whose LastDiff names the artifact it just wrote — numbered by
// [graph.RunContext.Step], so a fix-loop retry cannot overwrite the record of
// the attempt before it — and whose ChangedFiles is the verified,
// deduplicated, sorted change set. An empty change set is a valid outcome,
// not an error; see [Node.Run].
package write
