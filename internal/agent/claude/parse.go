//go:build unix

package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrUnparsableOutput reports that the CLI's stdout was not the single JSON
// object `--output-format json` promises.
//
// It is deliberately distinct from an invocation failure: a process that ran,
// exited zero, and printed something unreadable is a version-skew or
// truncation problem for a maintainer to look at, not a failing task for the
// agent to retry against. Recover the detail with errors.As and *ParseError.
var ErrUnparsableOutput = errors.New("belay/claude: cannot parse claude output")

// snippetLen bounds how much of an unparsable body a *ParseError quotes.
//
// The body is redacted stdout, so it is safe to show, but it can be megabytes
// of a truncated transcript. A few hundred bytes is enough for a maintainer to
// recognise "this is HTML, the proxy intercepted us" or "this stops
// mid-string".
const snippetLen = 240

// ParseError reports that the CLI's stdout could not be decoded into a result
// object.
//
// It wraps ErrUnparsableOutput, so errors.Is(err, ErrUnparsableOutput) holds,
// and the underlying *json.SyntaxError or *json.UnmarshalTypeError when there
// was one.
type ParseError struct {
	// Snippet is the leading, already-redacted bytes of what was actually
	// read, truncated to a length safe to log.
	Snippet string
	// Truncated reports that the capture hit its byte cap, which by itself
	// explains a body that stops mid-token.
	Truncated bool
	// Err is the underlying decode failure, if any.
	Err error
}

// Error implements error.
func (e *ParseError) Error() string {
	var b strings.Builder
	b.WriteString("belay/claude: cannot parse --output-format json body")
	if e.Truncated {
		b.WriteString(" (output was truncated at the capture limit)")
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if e.Snippet != "" {
		fmt.Fprintf(&b, ": %q", e.Snippet)
	}
	return b.String()
}

// Unwrap reports both the sentinel and the underlying decode failure.
func (e *ParseError) Unwrap() []error {
	if e.Err != nil {
		return []error{ErrUnparsableOutput, e.Err}
	}
	return []error{ErrUnparsableOutput}
}

// result mirrors the object `claude -p "<prompt>" --output-format json` prints.
//
// Only the fields belay actually consumes are named. Everything else the CLI
// emits — and everything a future version adds — is ignored by
// encoding/json's default behaviour, which is the whole version-tolerance
// story: a new field must never turn a working run into a failed one. The
// original bytes survive verbatim in AgentResponse.Raw for anyone who needs
// what this struct dropped.
//
// Field names follow the documented result message:
// https://code.claude.com/docs/en/headless#get-structured-output and
// https://code.claude.com/docs/en/agent-sdk/typescript#sdkresultmessage
type result struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	NumTurns  int    `json:"num_turns"`

	// Result is the final assistant text. It is decoded loosely because the
	// SDK types it as `unknown`, so a future version returning a non-string
	// there must degrade to "unusual text", not to a failed parse.
	Result flexString `json:"result"`

	// TotalCostUSD is the CLI's own cost figure. It carries a Set flag so
	// that "the field was absent, or was there but unreadable" is
	// distinguishable from "the field said zero": the first means fall back
	// to the price table, and conflating them is exactly how a budget guard
	// silently starts reporting $0.
	//
	// The CLI computes it client-side from a bundled price table, so it is
	// an estimate of the bill rather than the bill itself. belay still
	// treats it as reported-not-estimated, because Usage.Estimated
	// distinguishes "the backend told us a number" from "we derived one".
	// https://code.claude.com/docs/en/agent-sdk/cost-tracking
	TotalCostUSD flexFloat `json:"total_cost_usd"`

	// Usage counts only the top-level agent loop; tokens spent inside
	// subagents are excluded. ModelUsage includes them, which is why it is
	// preferred when present.
	Usage usage `json:"usage"`

	// ModelUsage is the per-model breakdown. The CLI is the TypeScript
	// implementation and spells it camelCase; the Python SDK renames it to
	// model_usage. Both spellings are accepted so that a rename in either
	// direction is not a silent loss of the better token source.
	ModelUsage      map[string]modelUsage `json:"modelUsage"`
	ModelUsageSnake map[string]modelUsage `json:"model_usage"`
}

// usage is the cumulative token count on a result message.
type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// modelUsage is one entry of the per-model breakdown, in the CLI's camelCase.
//
// Its own costUSD is deliberately not decoded. belay takes cost from the
// top-level total_cost_usd or from nothing, so that Usage.Estimated has
// exactly two meanings; summing a partially-populated per-model cost would
// add a third, "some of it was reported", that no caller can act on.
type modelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
}

// flexFloat decodes a number that must never be able to fail a whole parse.
//
// A JSON number decodes normally. A numeric string decodes too, since that is
// the likeliest shape a future version would change to. Anything else — a
// null, an object, a non-numeric string, a NaN or infinity — leaves Set false,
// which the cost path reads as "the CLI reported nothing" and answers with the
// price-table fallback. Requirement: an unparseable cost field degrades the
// budget guard's precision, it does not fail the run.
type flexFloat struct {
	// Value is the recovered number, meaningful only when Set is true.
	Value float64
	// Set reports that a finite number was recovered.
	Set bool
}

// UnmarshalJSON implements json.Unmarshaler. It never returns an error.
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	// Checked before anything else: unmarshalling null into a float64 is a
	// successful no-op that leaves zero behind, which would otherwise be
	// recorded as a recovered $0.00 rather than as "the CLI said nothing".
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		f.set(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			f.set(n)
		}
	}
	return nil
}

func (f *flexFloat) set(n float64) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return
	}
	f.Value, f.Set = n, true
}

// perModel returns the per-model breakdown under whichever key carried it.
func (r *result) perModel() map[string]modelUsage {
	if len(r.ModelUsage) > 0 {
		return r.ModelUsage
	}
	return r.ModelUsageSnake
}

// flexString decodes a JSON value that the SDK documents as `unknown` but
// that has been a string in every CLI version belay has seen.
//
// A plain string decodes to itself. Anything else keeps its compact JSON
// rendering, so a future CLI that returns a structured result still yields
// usable text instead of failing the whole decode over one field.
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (s *flexString) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = flexString(str)
		return nil
	}
	*s = flexString(bytes.TrimSpace(b))
	return nil
}

// parseResult decodes the CLI's stdout into a result and the exact bytes it
// came from.
//
// truncated says the capture hit its cap, which is reported in the error
// because it explains an otherwise baffling mid-token failure.
func parseResult(stdout string, truncated bool) (result, json.RawMessage, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return result{}, nil, &ParseError{Truncated: truncated,
			Err: errors.New("no output on stdout")}
	}

	var res result
	dec := json.NewDecoder(strings.NewReader(trimmed))
	// Deliberately *not* DisallowUnknownFields: ignoring fields this
	// version has never heard of is the requirement, not a lapse.
	if err := dec.Decode(&res); err != nil {
		return result{}, nil, &ParseError{
			Snippet: snippet(trimmed), Truncated: truncated, Err: err}
	}

	// A valid object followed by more content is not the single object the
	// contract promises; treating it as one would silently drop output.
	// The likeliest cause is --output-format stream-json reaching this
	// parser, and a clear error beats decoding only the first line.
	if hasTrailing(dec) {
		return result{}, nil, &ParseError{
			Snippet: snippet(trimmed), Truncated: truncated,
			Err: errors.New("trailing content after the JSON object"),
		}
	}

	return res, json.RawMessage(trimmed), nil
}

// hasTrailing reports whether anything but whitespace follows the object the
// decoder just read. Any read error means there was nothing more to read.
func hasTrailing(dec *json.Decoder) bool {
	_, err := dec.Token()
	return err == nil
}

// snippet returns a short, valid-UTF-8 prefix of s for an error message.
func snippet(s string) string {
	if len(s) <= snippetLen {
		return s
	}
	cut := s[:snippetLen]
	// Trim a rune the cut may have split, so the message stays printable.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}
