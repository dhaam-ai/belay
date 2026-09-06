//go:build unix

package claude

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// ErrUnknownModel reports that a cost had to be estimated but no price entry
// matched the model.
//
// It exists so that the budget guard can never be handed a silent zero. When
// the CLI stops reporting total_cost_usd and belay cannot price the tokens
// either, the correct outcome is a loud failure: a run that keeps going with
// USD == 0 spends real money against a ledger that believes it is free, which
// is the exact failure ADR-0007 exists to prevent.
var ErrUnknownModel = errors.New("belay/claude: no price entry for model")

// PriceError reports that tokens were spent under a model with no price entry.
//
// It wraps ErrUnknownModel; recover it with errors.As to report which model
// needs adding to the table.
type PriceError struct {
	// Model is the model identifier that could not be priced.
	Model string
	// InputTokens and OutputTokens are the counts that went unpriced, so an
	// operator can see how much was at stake.
	InputTokens, OutputTokens int64
}

// Error implements error.
func (e *PriceError) Error() string {
	name := e.Model
	if name == "" {
		name = "(unspecified)"
	}
	return fmt.Sprintf(
		"belay/claude: cannot price %d input and %d output tokens: "+
			"no price entry for model %q and the CLI reported no total_cost_usd",
		e.InputTokens, e.OutputTokens, name)
}

// Unwrap returns ErrUnknownModel.
func (e *PriceError) Unwrap() error { return ErrUnknownModel }

// Price is the list price of one model in US dollars per million tokens.
//
// Only the two base rates are stored. The cache tiers are derived from Input
// by the published multipliers below rather than stored per model, because
// they move together: a table with four independent numbers per model is four
// chances to mistype one and no extra fidelity.
type Price struct {
	// Input is the base input-token rate, USD per million tokens.
	Input float64
	// Output is the output-token rate, USD per million tokens.
	Output float64
}

// Cache-tier multipliers applied to Price.Input.
//
// Source: https://platform.claude.com/docs/en/build-with-claude/prompt-caching
// — "5-minute cache write tokens are 1.25 times the base input tokens price"
// and "Cache read tokens are 0.1 times the base input tokens price".
//
// belay assumes the 5-minute TTL because that is the Agent SDK's default for
// API-key authentication; a deployment that sets ENABLE_PROMPT_CACHING_1H
// pays 2x on writes and will be under-estimated here. That only matters on
// the fallback path, and under-estimating cache *writes* while the far larger
// cache-read volume is priced correctly is a small error in a number already
// flagged Estimated.
const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.10
)

// tokensPerMillion converts the per-million-token rates above to per-token.
const tokensPerMillion = 1_000_000.0

// defaultPrices is the fallback price table: list prices in USD per million
// tokens, keyed by the model identifiers the CLI reports and the short aliases
// it accepts on --model.
//
// This table is a static snapshot and will drift. It is consulted only when
// the CLI did not report a cost of its own, so drift degrades an already
// lower-confidence number rather than corrupting a good one; Usage.Estimated
// is how a caller knows which it is holding.
//
// Source: the model and pricing table in the bundled claude-api skill
// (cached 2026-06-24), which matches
// https://platform.claude.com/docs/en/about-claude/pricing
var defaultPrices = map[string]Price{
	"claude-fable-5":    {Input: 10, Output: 50},
	"claude-mythos-5":   {Input: 10, Output: 50},
	"claude-opus-5":     {Input: 5, Output: 25},
	"claude-opus-4-8":   {Input: 5, Output: 25},
	"claude-opus-4-7":   {Input: 5, Output: 25},
	"claude-opus-4-6":   {Input: 5, Output: 25},
	"claude-sonnet-5":   {Input: 2, Output: 10},
	"claude-sonnet-4-6": {Input: 3, Output: 15},
	"claude-haiku-4-5":  {Input: 1, Output: 5},

	// The CLI accepts these short aliases on --model, and belay's own
	// config default is the bare "sonnet". An alias is a moving target —
	// it names whichever model is current — so these entries are a best
	// effort that must be revisited whenever the table above gains a tier.
	"fable":  {Input: 10, Output: 50},
	"opus":   {Input: 5, Output: 25},
	"sonnet": {Input: 2, Output: 10},
	"haiku":  {Input: 1, Output: 5},
}

// DefaultPrices returns a copy of the built-in fallback price table.
//
// The result is a fresh map; mutating it does not affect later calls.
func DefaultPrices() map[string]Price {
	out := make(map[string]Price, len(defaultPrices))
	for k, v := range defaultPrices {
		out[k] = v
	}
	return out
}

// lookupPrice finds the price for a model identifier.
//
// It tries the identifier verbatim, then a single normalisation pass, and then
// gives up. It deliberately does not guess by prefix: pricing an unrecognised
// "claude-opus-6" at Opus 5 rates would be a silent, confident wrong answer,
// and a loud ErrUnknownModel is the better failure.
func lookupPrice(table map[string]Price, model string) (Price, bool) {
	if p, ok := table[model]; ok {
		return p, true
	}
	if norm := normalizeModel(model); norm != model {
		if p, ok := table[norm]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// normalizeModel strips the decorations a model identifier picks up on its way
// through a provider, leaving the bare id the price table is keyed on.
//
// Two forms are handled, both seen in real model identifiers: a provider
// prefix ("anthropic.claude-opus-5", as Bedrock spells it) and a dated
// snapshot suffix ("claude-opus-4-5@20251101"). Nothing else is guessed.
func normalizeModel(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	if i := strings.LastIndex(m, "."); i >= 0 && strings.HasSuffix(m[:i+1], "anthropic.") {
		m = m[i+1:]
	}
	if i := strings.IndexByte(m, '@'); i >= 0 {
		m = m[:i]
	}
	return m
}

// tokenCounts is the billable token volume of one invocation, split by tier
// because the tiers are priced differently.
type tokenCounts struct {
	input, output, cacheWrite, cacheRead int64
}

// total reports every token in the bucket, for the "was anything spent at
// all?" question.
func (t tokenCounts) total() int64 {
	return t.input + t.output + t.cacheWrite + t.cacheRead
}

// billableInput reports the input-side tokens belay attributes to the call.
//
// Cache writes and cache reads are input tokens: they are prompt and context
// the model processed, differing only in what they cost. belay.Usage has one
// input counter, so they are summed into it — reporting only input_tokens
// would understate a cache-heavy agent run by an order of magnitude.
func (t tokenCounts) billableInput() int64 {
	return t.input + t.cacheWrite + t.cacheRead
}

// cost prices the bucket at the given rates.
func (t tokenCounts) cost(p Price) float64 {
	inputRate := p.Input / tokensPerMillion
	return float64(t.input)*inputRate +
		float64(t.cacheWrite)*inputRate*cacheWriteMultiplier +
		float64(t.cacheRead)*inputRate*cacheReadMultiplier +
		float64(t.output)*(p.Output/tokensPerMillion)
}

// add accumulates another bucket.
func (t *tokenCounts) add(o tokenCounts) {
	t.input += o.input
	t.output += o.output
	t.cacheWrite += o.cacheWrite
	t.cacheRead += o.cacheRead
}

// countsByModel splits a result's token counts by the model that spent them.
//
// The per-model breakdown is preferred because it is the only field that
// counts subagent requests; the top-level usage object excludes them, so a
// graph node whose agent delegated would be under-counted. When there is no
// breakdown, everything is attributed to the model the request asked for.
// https://code.claude.com/docs/en/agent-sdk/cost-tracking
func countsByModel(res result, requestModel string) map[string]tokenCounts {
	if per := res.perModel(); len(per) > 0 {
		out := make(map[string]tokenCounts, len(per))
		for name, mu := range per {
			out[name] = tokenCounts{
				input:      mu.InputTokens,
				output:     mu.OutputTokens,
				cacheWrite: mu.CacheCreationInputTokens,
				cacheRead:  mu.CacheReadInputTokens,
			}
		}
		return out
	}
	return map[string]tokenCounts{
		requestModel: {
			input:      res.Usage.InputTokens,
			output:     res.Usage.OutputTokens,
			cacheWrite: res.Usage.CacheCreationInputTokens,
			cacheRead:  res.Usage.CacheReadInputTokens,
		},
	}
}

// usageOf converts a decoded result into a belay.Usage, reporting the CLI's
// own cost when it gave one and estimating from table otherwise.
//
// The three outcomes, in the order they are tried:
//
//  1. total_cost_usd was present and positive: USD is that figure verbatim
//     and Estimated is false.
//  2. It was absent, unreadable, or non-positive, and tokens were spent: USD
//     is priced from table and Estimated is true. A model with no entry is a
//     *PriceError, never a zero.
//  3. It was absent and no tokens were spent either: USD is zero and
//     Estimated is true — nothing to price, but nothing confirmed either.
//
// Case 2's "non-positive" clause matters more than it looks. The CLI zeroes
// every cost field on the result it emits after a session crash, so a run
// that really did spend money can report $0.00 with its token counts intact.
// Treating that as an exact zero would let the ledger lose the whole node.
func usageOf(res result, requestModel string, table map[string]Price) (belay.Usage, error) {
	buckets := countsByModel(res, requestModel)

	var totals tokenCounts
	for _, c := range buckets {
		totals.add(c)
	}

	out := belay.Usage{
		InputTokens:  totals.billableInput(),
		OutputTokens: totals.output,
	}

	if res.TotalCostUSD.Set && res.TotalCostUSD.Value > 0 {
		out.USD = res.TotalCostUSD.Value
		return out, nil
	}

	out.Estimated = true
	if totals.total() == 0 {
		return out, nil
	}

	for name, c := range buckets {
		p, ok := lookupPrice(table, name)
		if !ok {
			return belay.Usage{}, &PriceError{
				Model:        name,
				InputTokens:  c.billableInput(),
				OutputTokens: c.output,
			}
		}
		out.USD += c.cost(p)
	}
	return out, nil
}
