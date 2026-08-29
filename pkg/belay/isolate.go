package belay

import "context"

// Isolator creates and destroys an isolated workspace for one agent run —
// or one candidate within a best-of-N fanout — so concurrent or retried
// work never collides on the same files.
//
// A concrete Isolator might copy src to a temporary directory, check out a
// git worktree, or start a container; pkg/belay defines only the contract.
type Isolator interface {
	// Create produces a workspace derived from src (typically the target
	// repository) identified by id. id is caller-chosen and must be unique
	// among concurrently live workspaces — a run ID, a candidate index, a
	// node name — and an Isolator may use it to name a directory, branch
	// or container, but must not assume any particular format.
	//
	// Create must leave nothing behind on error: a non-nil error means the
	// returned Workspace is the zero value, and there is nothing for the
	// caller to Destroy.
	Create(ctx context.Context, src, id string) (Workspace, error)

	// Destroy releases everything Create allocated for ws.
	//
	// Destroy must be able to act on a Workspace decoded back from a run
	// journal after a process restart, not only one freshly returned by
	// Create in the same process — see Workspace. Callers call Destroy at
	// most once per successfully created Workspace; an Isolator is not
	// required to tolerate a second call.
	Destroy(ctx context.Context, ws Workspace) error
}

// Workspace is an isolated working copy an Isolator created for one agent
// run or candidate.
//
// Workspace is plain data with no behavior: it holds no cleanup closure and
// no live handle, deliberately. A Workspace value is written to the run
// journal after every node so a crashed run can resume, and only pure data
// survives that JSON round trip — a function-typed Cleanup field would
// silently vanish on decode, and the crash a journal exists to survive is
// exactly the moment that would matter. Releasing a Workspace's resources
// is always done by calling Isolator.Destroy(ctx, ws) again, using a
// Workspace decoded from the journal if the original process is gone; an
// Isolator implementation must be able to reconstruct everything it needs
// to destroy a workspace from these fields alone.
type Workspace struct {
	// ID is the identifier passed to Isolator.Create, echoed back so a
	// caller holding only a Workspace can name it in logs without
	// threading the original id alongside it.
	ID string `json:"id"`

	// Dir is the absolute path an AgentBackend, TestRunner or Linter
	// should treat as the working directory and repository root for this
	// workspace.
	Dir string `json:"dir"`

	// Ephemeral reports whether Destroy actually removes Dir. An Isolator
	// that hands back the original source tree unmodified — a
	// "no isolation" mode, useful for local debugging where copy overhead
	// is not worth paying — sets this false, so a caller knows not to
	// expect Dir to be gone after Destroy and, more importantly, knows not
	// to let concurrent candidates share it.
	Ephemeral bool `json:"ephemeral"`
}
