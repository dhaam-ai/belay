//go:build unix

// Package aireview is belay's AI-augmented quality gate: the review.mode
// "ai" path of ADR 0008, which replaces the deterministic scanner with a
// reviewer that QUERIES SonarQube through Model Context Protocol tool calls
// while producing a state structure identical to the deterministic one's.
//
// It ships two types, deliberately separate:
//
//   - [Reviewer] implements belay.Reviewer. It opens one MCP session per
//     review, asks get_project_quality_gate_status for the server's verdict
//     and search_sonar_issues_in_projects for the findings behind it, and
//     assembles one belay.QualityReport.
//   - [Node] implements graph.Node under the name graph.NodeReview, so it
//     can be registered in place of internal/nodes/review and route a run
//     the same three ways.
//
// # The point of the exercise: identical state, different reasoning
//
// The gate's verdict now comes from a conversation with a server rather
// than from a process's exit status, but nothing downstream may be able to
// tell. Every report this package returns goes through one constructor,
// [finalize], whose field-population rules are copied verbatim from
// internal/sonar/scanner's — same Source discipline, same never-nil Issues,
// same Counts-is-exactly-the-histogram, same always-valid Raw, same
// never-empty Summary, same never-GateUnknown Gate — so the two adapters'
// reports serialize to one top-level key set with Source the only permitted
// difference. See finalize's doc comment for the rules themselves.
//
// # Three outcomes, and only some of them are errors
//
// This mirrors internal/nodes/review exactly, because the graph must not be
// able to tell the two apart:
//
//   - The gate passed: GatePass, nil error; [Node] routes to graph.End.
//   - The gate failed: GateFail, nil error; [Node] routes to graph.NodeFix.
//     A failed gate is the ordinary branch to the fix loop, not a
//     malfunction.
//   - No verdict was reached — the server answered "NONE" (no analysis has
//     run), the MCP server was unreachable, docker is not installed, the
//     deployment did not advertise the tool, the context was cancelled:
//     GateError, and [Node] returns an error with Next left empty. Routing
//     here would spend a budgeted fix attempt asking a coding agent to
//     repair a tool failure it cannot see, and would do it again next pass.
//
// belay.Reviewer draws one more line inside that third case, and this
// package honors it: GateError with a NIL error means the tools ran and
// SonarQube simply had no verdict to give; GateError with a non-nil error
// means belay could not complete the review at all. Both are unresolved,
// but only the second is a malfunction.
//
// # Which tools, and which deliberately not
//
// get_project_quality_gate_status supplies the gate;
// search_sonar_issues_in_projects supplies the issues. analyze_code_snippet
// is deliberately not called: its own response carries a deprecationNotice
// field (see internal/sonar/mcp's AnalyzeCodeSnippetResponse), it would
// require per-file language detection belay does not have, and it would
// re-analyze snippets the server has already analyzed and already scored
// its gate against — producing findings that could contradict the verdict
// reported beside them. list_quality_gates and get_raw_source describe and
// fetch rather than judge, and nothing in a gate verdict needs them.
//
// # What is NOT scoped, and why
//
// belay.ReviewRequest.ChangedFiles is recorded in Raw and then not used as
// a filter. A SonarQube quality gate is evaluated by the server over the
// project (or its new-code period); dropping every issue outside
// ChangedFiles would leave a report whose issue list contradicts the gate
// printed beside it. belay.Reviewer explicitly permits whole-project
// analysis, and internal/sonar/scanner made the same call for the same
// reason.
//
// # Issue paging, and the one thing it can cost
//
// The search tool pages. This package walks up to [maxIssuePages] pages of
// [issuePageSize] and records Truncated in Raw when it stops early. If that
// cap is ever hit, the issue list is incomplete and belay's own fail_on
// scan of it is therefore best-effort — but the server's gate, which was
// computed over every issue, is not, and a failing server gate always
// produces GateFail regardless of what paging found. The direction that
// stays exposed is narrow and stated: a server gate that passes while an
// issue above belay's stricter threshold sits past page 10.
//
// # No network, no Docker, in tests
//
// [Session] is the seam. Its one real implementation is *mcp.Client, and
// mcp.Client is itself built on an injectable mcp.Transport, so every
// behavior in this package's tests is driven either against a fake Session
// or against a real mcp.Client speaking to an in-memory fake transport fed
// hand-authored JSON-RPC frames from testdata/. No test opens a socket,
// starts a container, or reads a real SONAR_TOKEN. The one test that proves
// a missing docker binary surfaces as belay.ErrToolchainMissing points
// [WithDockerBinary] at a path inside t.TempDir() that does not exist, so
// the failure happens in the kernel's exec lookup and no process is ever
// created.
//
// # Persistence
//
// Per ADR 0002 [Node] writes no state.json, no manifest.json and no
// journal. It returns a state.Patch carrying the Review block and writes
// one artifact, review-<step>.json, whose path travels in
// state.Review.ReportPath.
package aireview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/journal"
	"github.com/dhaam-ai/belay/internal/sonar/mcp"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// Paging bounds for search_sonar_issues_in_projects.
//
// issuePageSize is the tool's documented maximum (the parameter accepts
// (0, 500]); maxIssuePages caps one review at 5000 issues so a pathological
// project cannot turn one gate check into an unbounded conversation. See
// the package doc for what hitting the cap does and does not put at risk.
const (
	issuePageSize = 500
	maxIssuePages = 10
)

// openIssueStatuses are the issue statuses this package gates on.
//
// Issues a human has already triaged away — FALSE_POSITIVE, ACCEPTED — and
// issues the analysis has already seen fixed must not fail a gate or be
// handed to the fix node as work, so only the two live statuses are
// requested. The values are the vocabulary
// search_sonar_issues_in_projects's own issueStatuses parameter documents.
var openIssueStatuses = []string{"OPEN", "CONFIRMED"}

// Errors returned by this package.
//
// None of them means "the gate was evaluated and failed": that outcome is a
// report with Gate == belay.GateFail and a nil error, and deliberately has
// no sentinel, so no caller can mistake it for a malfunction.
var (
	// ErrMissingProjectKey reports a belay.ReviewRequest with no
	// ProjectKey. Every SonarQube tool this package calls is scoped to a
	// project; there is no whole-server review to fall back to.
	ErrMissingProjectKey = errors.New("belay/aireview: review request has no project key")

	// ErrMissingWorkDir reports a belay.ReviewRequest with no WorkDir.
	ErrMissingWorkDir = errors.New("belay/aireview: review request has no work dir")

	// ErrUnknownMode reports a review.ai.mcp.mode this package cannot
	// dial. Configuration validation rejects it first; reaching here with
	// one is a wiring bug.
	ErrUnknownMode = errors.New("belay/aireview: unknown mcp mode")

	// ErrMissingURL reports a "cloud" or "server" deployment with no
	// review.ai.mcp.url.
	//
	// "cloud" has no built-in default endpoint here on purpose. Guessing a
	// plausible-looking hostname for SonarQube Cloud's MCP endpoint would
	// turn a configuration mistake into a connection attempt against
	// whatever does answer that name, which is a worse failure than
	// refusing to dial.
	ErrMissingURL = errors.New("belay/aireview: mcp mode needs review.ai.mcp.url")

	// ErrMissingImage reports a "docker" deployment with no
	// review.ai.mcp.image.
	ErrMissingImage = errors.New("belay/aireview: mcp mode \"docker\" needs review.ai.mcp.image")

	// ErrGateUnresolved reports that the reviewer ran but reached no
	// verdict — belay.GateError. [Node] returns it rather than routing to
	// graph.NodeFix; see the package doc.
	ErrGateUnresolved = errors.New("belay/aireview: quality gate reached no verdict")
)

// Session is one initialized MCP conversation with a SonarQube MCP server:
// the two tool calls this package makes, plus the release.
//
// It is deliberately narrower than *mcp.Client, which also exposes the
// handshake, tools/list and the three tools this package does not use.
// Narrowing it here is what lets a test supply a scripted Session in four
// lines, and it documents at the type level exactly how much of SonarQube
// this reviewer is allowed to touch. A Session handed to [WithSession] must
// already be initialized: [Dialer] owns the handshake.
type Session interface {
	// GetProjectQualityGateStatus calls the tool of the same name.
	GetProjectQualityGateStatus(ctx context.Context, req mcp.GetProjectQualityGateStatusRequest) (mcp.GetProjectQualityGateStatusResponse, error)
	// SearchSonarIssuesInProjects calls the tool of the same name.
	SearchSonarIssuesInProjects(ctx context.Context, req mcp.SearchSonarIssuesInProjectsRequest) (mcp.SearchSonarIssuesInProjectsResponse, error)
	// Close releases the session's transport.
	Close() error
}

// *mcp.Client is the one real Session. The assertion is here rather than in
// a test so a change to either side is a compile error, not a late failure.
var _ Session = (*mcp.Client)(nil)

// Dialer opens one Session per review and is responsible for the MCP
// handshake: a Session it returns must already have completed initialize
// and tools/list, because [Reviewer] calls tools immediately.
//
// It is the injection point every test in this package uses instead of a
// network. Returning an error wrapping belay.ErrToolchainMissing from a
// Dialer is how "the software is not installed" reaches the graph.
type Dialer func(ctx context.Context) (Session, error)

// Reviewer implements belay.Reviewer against a SonarQube MCP server.
//
// One Reviewer is safe to reuse across reviews and across goroutines: it
// holds only configuration, and every review dials, uses and closes its own
// Session rather than sharing one (mcp.Client serializes calls, so sharing
// would silently serialize concurrent reviews).
type Reviewer struct {
	mcp    config.MCP
	dial   Dialer
	logger *slog.Logger

	httpClient   *http.Client
	dockerBinary string
	lookupEnv    func(string) (string, bool)
}

var _ belay.Reviewer = (*Reviewer)(nil)

// Option configures a Reviewer. See [New].
type Option func(*Reviewer)

// WithDialer replaces how a session is opened, bypassing mcp entirely. It
// is how every test in this package avoids a network and a container.
func WithDialer(d Dialer) Option {
	return func(r *Reviewer) { r.dial = d }
}

// WithSession makes every review use s, which must already be initialized.
// It is WithDialer's convenience form for a test with one scripted session;
// note that [Reviewer.Review] closes the session it is given, so a Session
// shared across two reviews must tolerate a second Close.
func WithSession(s Session) Option {
	return WithDialer(func(context.Context) (Session, error) { return s, nil })
}

// WithLogger sets the structured logger. Nil uses slog.Default.
func WithLogger(l *slog.Logger) Option {
	return func(r *Reviewer) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithHTTPClient sets the http.Client the "cloud" and "server" deployments
// POST with. Nil uses http.DefaultClient.
func WithHTTPClient(c *http.Client) Option {
	return func(r *Reviewer) { r.httpClient = c }
}

// WithDockerBinary overrides the container runtime the "docker" deployment
// starts, which defaults to [DefaultDockerBinary].
//
// It exists for a host whose runtime is podman, or whose docker lives
// somewhere unusual — and, in this package's own tests, to point at a path
// that provably does not exist so the missing-toolchain translation can be
// proven without ever creating a process.
func WithDockerBinary(path string) Option {
	return func(r *Reviewer) {
		if strings.TrimSpace(path) != "" {
			r.dockerBinary = path
		}
	}
}

// WithLookupEnv replaces os.LookupEnv, which is how credentials reach a
// transport. Nil restores os.LookupEnv.
func WithLookupEnv(f func(string) (string, bool)) Option {
	return func(r *Reviewer) {
		if f != nil {
			r.lookupEnv = f
		}
	}
}

// DefaultDockerBinary is the container runtime the "docker" deployment
// starts when [WithDockerBinary] is not used.
const DefaultDockerBinary = "docker"

// forwardedEnv names the environment variables passed through to a
// containerized MCP server so it can find and address its SonarQube.
//
// They are forwarded by NAME with `docker run -e NAME`, never as
// `-e NAME=value`: a value on argv is visible in every process listing on
// the host, and SONAR_TOKEN is one of these. internal/sonar/mcp's
// StdioTransport puts the token into the child's own environment, where
// docker then reads it.
var forwardedEnv = []string{mcp.SecretEnvName, "SONAR_HOST_URL", "SONAR_ORGANIZATION"}

// New returns a Reviewer that reaches its SonarQube MCP server as m
// describes — config.Review.AI.MCP, passed through by the wiring layer
// rather than read from disk here.
func New(m config.MCP, opts ...Option) *Reviewer {
	r := &Reviewer{
		mcp:          m,
		logger:       slog.Default(),
		dockerBinary: DefaultDockerBinary,
		lookupEnv:    os.LookupEnv,
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.dial == nil {
		r.dial = r.dialMCP
	}
	return r
}

// Review implements belay.Reviewer.
//
// It opens one session, asks SonarQube for the project's quality gate
// status and for the issues behind it, and returns one report. A failed
// gate is a successful call — (Gate: GateFail, nil) — because the graph
// routes it to the fix node exactly like a failing test.
//
// The returned report is always fully populated, including on every error
// path, so a caller can journal it before it inspects the error and so its
// serialized key set never depends on the outcome. See [finalize] for the
// exact rules.
func (r *Reviewer) Review(ctx context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
	started := time.Now()
	raw := RawReview{
		Mode:         string(r.mcp.Mode),
		Endpoint:     r.endpoint(),
		ProjectKey:   req.ProjectKey,
		ChangedFiles: slices.Clone(req.ChangedFiles),
	}

	status, issues, err := r.review(ctx, req, &raw)
	raw.DurationMS = time.Since(started).Milliseconds()

	report := finalize(status, issues, raw, req.FailOn, err)
	r.logger.LogAttrs(ctx, slog.LevelDebug, "belay/aireview: review finished",
		slog.String("mode", string(r.mcp.Mode)),
		slog.String("project_key", req.ProjectKey),
		slog.String("server_status", status),
		slog.String("gate", report.Gate.String()),
		slog.Int("issues", len(report.Issues)),
		slog.Duration("duration", time.Since(started)))
	return report, err
}

// review is Review's fallible middle: it returns SonarQube's own status
// word and the mapped issues, leaving every report-shaping rule to
// [finalize] so that no early return can produce a differently shaped
// report from a late one.
func (r *Reviewer) review(ctx context.Context, req belay.ReviewRequest, raw *RawReview) (string, []belay.Issue, error) {
	if err := r.validate(ctx, req); err != nil {
		return "", nil, err
	}

	session, err := r.dial(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("belay/aireview: open mcp session: %w", err)
	}
	defer func() {
		if cerr := session.Close(); cerr != nil {
			r.logger.LogAttrs(ctx, slog.LevelWarn, "belay/aireview: closing mcp session",
				slog.String("error", cerr.Error()))
		}
	}()

	status, err := r.gateStatus(ctx, session, req, raw)
	if err != nil {
		return "", nil, err
	}
	// A server that has no verdict has no analysis behind it either, so
	// there is nothing to search for. Returning here keeps a GateError
	// report empty rather than attaching issues to a verdict that does not
	// exist.
	if mcp.BelayGate(status) == belay.GateError {
		return status, nil, nil
	}

	issues, err := r.searchIssues(ctx, session, req, raw)
	if err != nil {
		return status, nil, err
	}
	return status, issues, nil
}

// validate rejects a request this package cannot act on, and a context that
// is already done.
func (r *Reviewer) validate(ctx context.Context, req belay.ReviewRequest) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("belay/aireview: %w", err)
	}
	if strings.TrimSpace(req.WorkDir) == "" {
		return ErrMissingWorkDir
	}
	if strings.TrimSpace(req.ProjectKey) == "" {
		return ErrMissingProjectKey
	}
	return nil
}

// gateStatus calls get_project_quality_gate_status and returns SonarQube's
// own status word verbatim, recording the conditions and the raw result.
func (r *Reviewer) gateStatus(ctx context.Context, s Session, req belay.ReviewRequest, raw *RawReview) (string, error) {
	resp, err := s.GetProjectQualityGateStatus(ctx, mcp.GetProjectQualityGateStatusRequest{
		ProjectKey: req.ProjectKey,
	})
	raw.QualityGateRaw = rawOrNull(resp.Raw)
	if err != nil {
		return "", fmt.Errorf("belay/aireview: get_project_quality_gate_status: %w", err)
	}
	raw.Conditions = append(raw.Conditions, resp.ProjectStatus.Conditions...)
	return resp.ProjectStatus.Status, nil
}

// searchIssues pages search_sonar_issues_in_projects and maps every issue
// into belay's vocabulary. See the package doc for the paging bounds.
func (r *Reviewer) searchIssues(ctx context.Context, s Session, req belay.ReviewRequest, raw *RawReview) ([]belay.Issue, error) {
	issues := make([]belay.Issue, 0, issuePageSize)
	for page := 1; page <= maxIssuePages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("belay/aireview: %w", err)
		}
		resp, err := s.SearchSonarIssuesInProjects(ctx, mcp.SearchSonarIssuesInProjectsRequest{
			ProjectKeys:   []string{req.ProjectKey},
			IssueStatuses: slices.Clone(openIssueStatuses),
			PageIndex:     page,
			PageSize:      issuePageSize,
		})
		raw.Pages = page
		raw.IssuesRaw = append(raw.IssuesRaw, rawOrNull(resp.Raw))
		if err != nil {
			return nil, fmt.Errorf("belay/aireview: search_sonar_issues_in_projects: %w", err)
		}
		raw.IssuesTotal = resp.Total
		for _, si := range resp.Issues {
			issues = append(issues, mapIssue(si, req.ProjectKey))
		}
		if len(resp.Issues) < issuePageSize {
			break
		}
		if resp.Total > 0 && len(issues) >= resp.Total {
			break
		}
		if page == maxIssuePages {
			raw.Truncated = true
		}
	}
	return issues, nil
}

// endpoint names the deployment in Raw: the URL for an HTTP mode, the image
// for docker. Neither ever carries a credential.
func (r *Reviewer) endpoint() string {
	if r.mcp.Mode == config.MCPModeDocker {
		return r.mcp.Image
	}
	return r.mcp.URL
}

// dialMCP is the default Dialer: it builds the transport config.MCP.Mode
// selects, wraps it in an mcp.Client, and completes the handshake and the
// tools/list gate before returning.
//
// Both are done here rather than lazily inside Review because mcp.Client
// refuses to call a tool before either has happened, so a session that
// reaches Review is one whose tools are known to be callable — and a
// deployment missing a tool then fails at the call site with
// mcp.ErrToolNotAdvertised, naming the tool, rather than at dial time with
// a list the caller has to diff by hand.
func (r *Reviewer) dialMCP(ctx context.Context) (Session, error) {
	transport, err := r.transport(ctx)
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(transport, r.logger)
	if _, err := client.Initialize(ctx); err != nil {
		return nil, errors.Join(err, client.Close())
	}
	if _, err := client.ListTools(ctx); err != nil {
		return nil, errors.Join(err, client.Close())
	}
	return client, nil
}

// transport builds the mcp.Transport for the configured deployment.
//
// SONAR_TOKEN is read here, from belay's own environment, and handed to the
// transport as a header value or a child environment variable. It is never
// placed on a command line and never logged; internal/sonar/mcp seeds a
// redactor with it so even a server that echoes it back cannot get it into
// an error string.
func (r *Reviewer) transport(ctx context.Context) (mcp.Transport, error) {
	token, _ := r.lookupEnv(mcp.SecretEnvName)

	switch r.mcp.Mode {
	case config.MCPModeCloud, config.MCPModeServer:
		if strings.TrimSpace(r.mcp.URL) == "" {
			return nil, fmt.Errorf("%w: mode %q", ErrMissingURL, r.mcp.Mode)
		}
		return &mcp.HTTPTransport{
			Client: r.httpClient,
			URL:    r.mcp.URL,
			Token:  token,
			Logger: r.logger,
		}, nil

	case config.MCPModeDocker:
		if strings.TrimSpace(r.mcp.Image) == "" {
			return nil, ErrMissingImage
		}
		t := mcp.NewStdioTransport(r.logger)
		if err := t.Start(ctx, mcp.StdioCommand{
			Path:     r.dockerBinary,
			Args:     dockerArgs(r.mcp.Image),
			EnvAllow: slices.Clone(forwardedEnv),
			Token:    token,
		}); err != nil {
			// Start already returns *belay.ToolchainError for a missing or
			// unexecutable runtime, so %w is what keeps
			// errors.Is(err, belay.ErrToolchainMissing) true all the way to
			// the graph — which decides "a human installs software" versus
			// "the agent edits code" on exactly that test.
			return nil, fmt.Errorf("belay/aireview: start mcp server: %w", err)
		}
		return t, nil

	default:
		return nil, fmt.Errorf("%w: %q (want %q, %q or %q)", ErrUnknownMode, r.mcp.Mode,
			config.MCPModeCloud, config.MCPModeDocker, config.MCPModeServer)
	}
}

// dockerArgs builds the argv for a containerized MCP server.
//
// -i keeps stdin open, which is the entire stdio transport; --rm stops a
// failed review from leaving a container behind on every run; each -e
// forwards a variable BY NAME from the docker process's own environment,
// which internal/sonar/mcp populates, so no credential is ever visible in a
// host process listing.
//
// UNVERIFIED: this argv follows the container conventions
// internal/sonar/mcp's StdioTransport documents and the image name
// config.defaults.go already ships, but it was not run against the real
// sonarsource/sonar-mcp-server image — this task forbids invoking Docker.
// The flags themselves are docker-run generic; what is unproven is only
// whether that image needs additional arguments.
func dockerArgs(image string) []string {
	args := make([]string, 0, 4+2*len(forwardedEnv))
	args = append(args, "run", "--rm", "-i")
	for _, name := range forwardedEnv {
		args = append(args, "-e", name)
	}
	return append(args, image)
}

// rawOrNull returns raw, or the JSON literal null when raw is empty, so
// RawReview never serializes an absent tool result as a bare nothing.
func rawOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// ---------------------------------------------------------------------
// Node: the graph.NodeReview substitute.
// ---------------------------------------------------------------------

// Node is the graph.Node form of this package: belay's quality gate, backed
// by SonarQube over MCP.
//
// It answers to graph.NodeReview, the same name internal/nodes/review
// answers to, so registering one or the other is the entire difference
// between review.mode "lint"/"sonar" and review.mode "ai". Its routing,
// its threshold handling, its artifact name and its state.Patch are
// deliberately identical to that node's; only the Reviewer underneath
// differs.
//
// Like its deterministic sibling it holds no per-run state, so one Node is
// safe to register once and re-run for every step of every run — which is
// what the dispatcher's crash-recovery contract requires of it.
type Node struct {
	reviewer belay.Reviewer
}

var _ graph.Node = (*Node)(nil)

// NewNode returns a review Node backed by r.
//
// A nil r is legitimate and means "resolve one at run time": Run then uses
// graph.RunContext.Reviewer if the dispatcher wired one, and otherwise
// builds a [Reviewer] from rc.Config.Review.AI.MCP. That ordering lets this
// node drop into a graph whose wiring already supplies a Reviewer, without
// requiring the wiring layer to know this package exists.
func NewNode(r belay.Reviewer) *Node { return &Node{reviewer: r} }

// Name implements graph.Node and returns graph.NodeReview.
func (*Node) Name() string { return graph.NodeReview }

// Run implements graph.Node: it runs the SonarQube quality gate and routes
// on the verdict.
//
// A passing gate ends the run (graph.End); a failing gate routes to
// graph.NodeFix. Both are (Result, nil). A non-nil error means no verdict
// was reached, and every such Result leaves Next empty so that no error path
// can route to the fix loop and burn a budgeted repair attempt on a tool
// failure no edit can fix.
//
// Result.Usage is always zero. belay.Reviewer reports no cost, and its only
// possible channel for one — QualityReport.Raw — is a field ADR 0006
// forbids the graph from reading. Reporting a fabricated zero is honest
// where inventing a number would not be.
func (n *Node) Run(ctx context.Context, rc *graph.RunContext) (graph.Result, error) {
	if rc == nil {
		return graph.Result{Status: journal.StatusFailed}, errors.New("belay/aireview: nil run context")
	}
	if err := ctx.Err(); err != nil {
		return graph.Result{Status: journal.StatusAborted}, fmt.Errorf("belay/aireview: %w", err)
	}

	failOn, err := belay.ParseSeverity(string(rc.Config.Review.FailOn))
	if err != nil {
		return graph.Result{Status: journal.StatusFailed},
			fmt.Errorf("belay/aireview: invalid review.fail_on: %w", err)
	}
	if rc.Workspace == "" {
		return graph.Result{Status: journal.StatusFailed}, errors.New("belay/aireview: run context has no workspace")
	}

	reviewer := n.reviewerFor(rc)
	report, err := reviewer.Review(ctx, belay.ReviewRequest{
		WorkDir: rc.Workspace,
		// Cloned rather than aliased: rc.State is a snapshot, but its
		// slice header still points at the dispatcher's backing array, and
		// an adapter that sorted or truncated it in place would rewrite the
		// blackboard through a value it was handed read-only.
		ChangedFiles: slices.Clone(rc.State.Code.ChangedFiles),
		ProjectKey:   filepath.Base(rc.Workspace),
		FailOn:       failOn,
	})
	if err != nil {
		return abortedOrFailed(err), fmt.Errorf("belay/aireview: reviewer: %w", err)
	}
	return n.verdict(rc, report, failOn)
}

// reviewerFor resolves which belay.Reviewer this run uses. See [NewNode].
func (n *Node) reviewerFor(rc *graph.RunContext) belay.Reviewer {
	switch {
	case n.reviewer != nil:
		return n.reviewer
	case rc.Reviewer != nil:
		return rc.Reviewer
	default:
		return New(rc.Config.Review.AI.MCP, WithLogger(rc.Logger))
	}
}

// abortedOrFailed picks the Status for an error path. A cancelled context
// cut the node short (StatusAborted); anything else is a node that ran and
// could not produce a verdict (StatusFailed). Next is empty either way.
func abortedOrFailed(err error) graph.Result {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return graph.Result{Status: journal.StatusAborted}
	}
	return graph.Result{Status: journal.StatusFailed}
}

// verdict resolves the gate, archives the report, and routes.
func (n *Node) verdict(rc *graph.RunContext, report belay.QualityReport, failOn belay.Severity) (graph.Result, error) {
	log := logger(rc)
	gate := resolveGate(report.Gate, report.Counts, failOn)
	if gate != report.Gate {
		log.Warn("aireview: adapter gate overridden by review.fail_on",
			slog.String("source", report.Source),
			slog.String("reported", report.Gate.String()),
			slog.String("resolved", gate.String()),
			slog.String("fail_on", failOn.String()))
	}
	report.Gate = gate
	if report.Issues == nil {
		// belay.QualityReport.Issues is contractually never nil, and this
		// package's own finalize guarantees it — but this node is the last
		// thing between ANY belay.Reviewer and the blackboard, including one
		// this package did not write. state.NewReview preserves nil exactly,
		// so a nil here would serialize as "issues": null in both the
		// artifact and state.json.
		report.Issues = []belay.Issue{}
	}

	path, err := writeReport(rc, report, log)
	if err != nil {
		return graph.Result{Status: journal.StatusFailed}, err
	}

	source := report.Source
	if source == "" {
		source = Source
	}
	note := summarizeVerdict(gate, source, report.Counts, failOn, path)
	blackboard := state.NewReview(report, path)
	patch := state.Patch{Review: &blackboard}

	log.Info("aireview: gate evaluated",
		slog.String("gate", gate.String()),
		slog.String("source", source),
		slog.String("fail_on", failOn.String()),
		slog.Int("at_or_above", report.Counts.AtOrAbove(failOn)),
		slog.Int("total_issues", report.Counts.Total()),
		slog.String("report", path))

	switch gate {
	case belay.GatePass:
		return graph.Result{Next: graph.End, Status: journal.StatusOK, Patch: patch, Note: note}, nil
	case belay.GateFail:
		return graph.Result{Next: graph.NodeFix, Status: journal.StatusOK, Patch: patch, Note: note}, nil
	default:
		// GateError. The patch and the note still travel, so a human
		// resuming the run can see what the reviewer managed to report, but
		// Next stays empty: an unresolved gate must not spend a fix attempt.
		return graph.Result{Status: journal.StatusFailed, Patch: patch, Note: note},
			fmt.Errorf("%w: %s reported %s at fail_on=%s (report %s)",
				ErrGateUnresolved, source, belay.GateError, failOn, path)
	}
}

// resolveGate reduces the adapter's reported gate to the verdict this node
// acts on. It is character for character the rule internal/nodes/review
// applies, so swapping one node for the other cannot change a route.
//
// The node's threshold wins over the adapter's own verdict. [Reviewer]
// already honors req.FailOn, so recomputing reproduces its answer whenever
// it kept its promise — and catches any belay.Reviewer, including one this
// package did not write, whose Gate its own Counts contradict.
//
// GateError and GateUnknown are the exception and are preserved as
// GateError. They are not verdicts about issues; they are the absence of
// one, and Counts cannot tell "measured, found nothing" apart from
// "measured nothing at all". Recomputing them would turn a reviewer that
// fell over into a clean pass — the single most dangerous mistranslation
// available here, since it ships unreviewed code silently.
func resolveGate(reported belay.GateStatus, counts belay.Counts, failOn belay.Severity) belay.GateStatus {
	if reported == belay.GateError || reported == belay.GateUnknown {
		return belay.GateError
	}
	if counts.AtOrAbove(failOn) > 0 {
		return belay.GateFail
	}
	return belay.GatePass
}

// writeReport archives the full report as review-<step>.json and returns its
// path relative to the run directory.
//
// The name matches internal/nodes/review's byte for byte: a run that
// switched review.mode between resumes must not leave two differently named
// artifacts for the same step.
func writeReport(rc *graph.RunContext, report belay.QualityReport, log *slog.Logger) (string, error) {
	name := fmt.Sprintf("review-%d.json", rc.Step)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		// Raw is the only field that can fail to marshal — it is opaque
		// bytes an adapter supplied. Dropping it is strictly better than
		// discarding a verdict belay already reached over a debug field
		// ADR 0006 forbids the graph from reading anyway.
		log.Warn("aireview: dropping unmarshalable Raw from report artifact",
			slog.String("source", report.Source), slog.String("error", err.Error()))
		report.Raw = nil
		if data, err = json.MarshalIndent(report, "", "  "); err != nil {
			return "", fmt.Errorf("belay/aireview: encode %s: %w", name, err)
		}
	}
	path, err := rc.WriteArtifact(name, append(data, '\n'))
	if err != nil {
		return "", fmt.Errorf("belay/aireview: write %s: %w", name, err)
	}
	return path, nil
}

// summarizeVerdict renders graph.Result.Note, for example
//
//	gate fail: 1 blocker, 3 major at fail_on=major (source=sonar-mcp-ai-review, report=artifacts/review-6.json)
//
// The wording is internal/nodes/review's, so a timeline reads the same
// whichever reviewer produced the run. It is redacted by construction:
// every part is a severity count, a constant, an adapter name or a path
// belay chose — never captured tool output. QualityReport.Summary is
// deliberately excluded for that reason.
func summarizeVerdict(gate belay.GateStatus, source string, c belay.Counts, failOn belay.Severity, path string) string {
	var head string
	switch gate {
	case belay.GatePass:
		head = fmt.Sprintf("gate pass: no issues at or above fail_on=%s", failOn)
	case belay.GateFail:
		head = fmt.Sprintf("gate fail: %s at fail_on=%s", breakdown(c, failOn), failOn)
	default:
		head = fmt.Sprintf("gate error: no verdict reached at fail_on=%s", failOn)
	}
	return fmt.Sprintf("%s (source=%s, report=%s)", head, source, path)
}

// logger returns the run context's logger, or a default.
func logger(rc *graph.RunContext) *slog.Logger {
	if rc.Logger != nil {
		return rc.Logger
	}
	return slog.Default()
}
