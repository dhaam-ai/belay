package replay

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// redacted replaces every secret Scrub finds.
const redacted = "[REDACTED]"

// credentialEnvs are the environment variables a live agent backend
// authenticates with: an API key, or a subscription's OAuth token. Scrub
// reads their current values (and never stores or logs them) so that if a
// backend's raw output accidentally echoes a real credential back, the
// literal value never reaches a committed cassette.
//
// This mirrors internal/agent/claude's own credential constants; they are
// re-declared here rather than imported, since internal/agent/claude is a
// //go:build unix package specific to one backend and this package must
// stay backend-agnostic and portable.
// #nosec G101 -- these are the *names* of environment variables, not
// credentials. Their values are read once per Scrub call via os.Getenv and
// never stored.
var credentialEnvs = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

var (
	// reAnthropicKey matches an Anthropic API key or OAuth token by their
	// shared "sk-ant-..." prefix, independent of whether it happens to match
	// a current credential environment value — a key baked into a
	// test fixture, or one quoted in an agent's own example output, has
	// no environment counterpart to match against.
	reAnthropicKey = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)

	// reBearerToken matches an HTTP Authorization-style bearer token.
	reBearerToken = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`)

	// reJWT matches a JSON Web Token: three base64url segments joined by
	// dots. Every JWT header starts "eyJ" (the base64 encoding of `{"`),
	// which anchors the match away from ordinary dotted text.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

	// reHomePath matches an absolute macOS or Linux home-directory path —
	// belay's two target platforms (see internal/agent/claude) — up to
	// its next whitespace or quote, so a username never reaches a
	// committed cassette.
	reHomePath = regexp.MustCompile(`(?:/Users|/home)/[^\s"'` + "`" + `]+`)
)

// Scrub redacts every secret shape this package knows how to recognize
// from s: the live ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN values if
// set in the current environment, any "sk-ant-..." key or token regardless
// of environment, bearer
// tokens, JWTs, and absolute home-directory paths. Backend applies it to
// every Interaction before Record appends it to a Cassette, because
// cassettes are meant to be committed to a public repository (ADR-0009).
//
// Scrub is a best-effort heuristic, not a guarantee: it recognizes the
// secret SHAPES named above, not every possible credential format. A
// maintainer reviewing a cassette diff before committing it remains the
// last line of defense.
func Scrub(s string) string {
	if s == "" {
		return s
	}
	for _, name := range credentialEnvs {
		if v := os.Getenv(name); v != "" {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	s = reAnthropicKey.ReplaceAllString(s, redacted)
	s = reBearerToken.ReplaceAllString(s, "Bearer "+redacted)
	s = reJWT.ReplaceAllString(s, redacted)
	s = reHomePath.ReplaceAllString(s, "~")
	return s
}

// scrubResponse returns a copy of resp with Text and Raw scrubbed.
//
// SessionID is left alone: it is an opaque backend-assigned identifier
// belay treats as non-sensitive — it is sent back to the very same
// backend on every subsequent --resume call, in Live and Record mode
// alike — not a credential.
func scrubResponse(resp belay.AgentResponse) belay.AgentResponse {
	out := resp
	out.Text = Scrub(resp.Text)
	if len(resp.Raw) > 0 {
		out.Raw = json.RawMessage(Scrub(string(resp.Raw)))
	}
	return out
}

// scrubRequest returns a copy of n with every free-text field scrubbed.
// WorkDir and SessionID are already fixed placeholders by the time
// Normalize produces n (see NormalizedRequest), so there is nothing
// sensitive left in them to scrub.
func scrubRequest(n NormalizedRequest) NormalizedRequest {
	out := n
	out.Prompt = Scrub(n.Prompt)
	out.SystemPrompt = Scrub(n.SystemPrompt)
	if len(n.MCPConfig) > 0 {
		out.MCPConfig = json.RawMessage(Scrub(string(n.MCPConfig)))
	}
	return out
}
