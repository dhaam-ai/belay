//go:build unix

package claude

import (
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/dhaam-ai/belay/pkg/belay"
)

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
// gives up. It deliberately does not guess by prefix or by family: pricing an
// unrecognised "claude-opus-6" at Opus 5 rates, or "claude-opus-4-1" at the
// rate of the opus models the table does list, would be a silent, confident
// wrong answer, and members of one family have differed threefold in price. A
// miss is answered by resolvePrice, at the highest rate the table knows.
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

// resolvePrice is the rate a model is charged at: its own when the table
// knows it (lookupPrice), and otherwise the highest rate the table holds, in
// each tier separately. known reports which.
//
// # Why the highest, and not a refusal
//
// belay once refused to start on any model its table did not list, on the
// reasoning that a ceiling it could not price was no ceiling. The reasoning
// was right and the remedy was wrong: the table is a snapshot, so every CLI
// release with a new model, every dated identifier and every alias the CLI
// accepts (sonnet[1m], opusplan) stopped belay outright, although the CLI
// normally reports its own cost and the table is only the fallback. Charging
// the dearest rate belay knows keeps what the refusal protected: the ledger
// is never handed a zero, and an error in the estimate errs towards stopping
// a run early, which a person can resume after raising the ceiling, never
// towards spending past it. Usage.Estimated says the figure is a guess, and
// the backend says, once per model, that it is one (warnUnpriced).
//
// "Highest" means highest in this table. A model dearer than anything belay
// lists is still under-charged on the fallback path; the warning names it so
// a person can see that, and adding it to the table is the remedy.
func resolvePrice(table map[string]Price, model string) (p Price, known bool) {
	if p, ok := lookupPrice(table, model); ok {
		return p, true
	}
	return highestPrice(table), false
}

// highestPrice is the highest input rate and the highest output rate in
// table, which need not come from one model.
func highestPrice(table map[string]Price) Price {
	var top Price
	for _, p := range table {
		top.Input = max(top.Input, p.Input)
		top.Output = max(top.Output, p.Output)
	}
	return top
}

// planAliases are CLI aliases that name more than one model, each mapped to
// the dearest model it can run. opusplan runs Opus while planning and Sonnet
// otherwise, and a ceiling can only rely on the dearer.
var planAliases = map[string]string{
	"opusplan": "opus",
}

// normalizeModel strips the decorations a model identifier picks up on its way
// through a provider or the CLI, leaving the bare id the price table is keyed
// on.
//
// The forms handled are all seen in real identifiers: a provider prefix
// ("anthropic.claude-opus-5", "us.anthropic.claude-opus-5", as Bedrock spells
// it), a Bedrock version suffix ("-v1:0"), a dated snapshot, either Vertex's
// ("claude-opus-4-5@20251101") or the API's ("claude-haiku-4-5-20251001"), a
// context-window suffix the CLI accepts on an alias ("sonnet[1m]"), and an
// alias that names several models (planAliases). Nothing else is guessed.
func normalizeModel(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	if i := strings.LastIndex(m, "."); i >= 0 && strings.HasSuffix(m[:i+1], "anthropic.") {
		m = m[i+1:]
	}
	if i := strings.IndexByte(m, '@'); i >= 0 {
		m = m[:i]
	}
	if i := strings.IndexByte(m, '['); i >= 0 && strings.HasSuffix(m, "]") {
		m = m[:i]
	}
	m = trimVersionSuffix(m)
	m = trimDateSuffix(m)
	if alias, ok := planAliases[m]; ok {
		m = alias
	}
	return m
}

// trimVersionSuffix removes a Bedrock model version, "-v1:0" or "-v2", from
// the end of m.
func trimVersionSuffix(m string) string {
	if i := strings.LastIndex(m, ":"); i >= 0 && allDigits(m[i+1:]) {
		m = m[:i]
	}
	if i := strings.LastIndex(m, "-v"); i >= 0 && allDigits(m[i+2:]) {
		m = m[:i]
	}
	return m
}

// trimDateSuffix removes a snapshot date, "-20251001", from the end of m.
func trimDateSuffix(m string) string {
	if i := strings.LastIndex(m, "-"); i >= 0 && len(m)-i-1 == 8 && allDigits(m[i+1:]) {
		return m[:i]
	}
	return m
}

// allDigits reports whether s is non-empty and every byte of it is a digit.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// warnedUnpriced holds the models this process has already warned about
// (warnUnpriced). It is package-level because the warning is about the price
// table, which is shared by every Backend in the process, and the point is to
// say it once rather than once per call or per Backend.
var warnedUnpriced sync.Map

// warnUnpriced tells the person, once per model per process, that model is
// not in the price table and is charged at the highest rate belay knows.
func warnUnpriced(logger *slog.Logger, model string, rate Price) {
	name := model
	if name == "" {
		name = "(unspecified)"
	}
	if _, seen := warnedUnpriced.LoadOrStore(name, true); seen {
		return
	}
	logger.Warn("belay/claude: belay has no price for this model, so wherever it has to price one itself it "+
		"assumes the most expensive rate it knows, which keeps the budget ceiling safe; add the model to "+
		"the price table for an exact figure",
		slog.String("model", name),
		slog.Float64("input_usd_per_mtok", rate.Input),
		slog.Float64("output_usd_per_mtok", rate.Output))
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
// own cost when it gave one and estimating from table otherwise. It also
// returns every model it had to charge at the highest rate the table knows,
// for the backend to warn about.
//
// The three outcomes, in the order they are tried:
//
//  1. total_cost_usd was present and positive: USD is that figure verbatim
//     and Estimated is false.
//  2. It was absent, unreadable, or non-positive, and tokens were spent: USD
//     is priced from table and Estimated is true. A model with no entry is
//     priced at the highest rate in the table (resolvePrice), never a zero.
//  3. It was absent and no tokens were spent either: USD is zero and
//     Estimated is true — nothing to price, but nothing confirmed either.
//
// Case 2's "non-positive" clause matters more than it looks. The CLI zeroes
// every cost field on the result it emits after a session crash, so a run
// that really did spend money can report $0.00 with its token counts intact.
// Treating that as an exact zero would let the ledger lose the whole node.
func usageOf(res result, requestModel string, table map[string]Price) (belay.Usage, []string) {
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

	var unpriced []string
	for name, c := range buckets {
		p, known := resolvePrice(table, name)
		if !known && c.total() > 0 {
			unpriced = append(unpriced, name)
		}
		out.USD += c.cost(p)
	}
	slices.Sort(unpriced)
	return out, unpriced
}

// ListPrice implements belay.ModelPricer: it reports what model lists at, in
// USD per million tokens, before anything has been spent on it.
//
// It reads the same table, through the same resolution, that usageOf prices
// a finished call with, so the rate a dry run shows for a node and the rate
// its spend is estimated at cannot come apart. A model that is not in the
// table is answered with the highest rate the table holds rather than an
// error, and the backend says so once (see resolvePrice, which argues why).
// It is never a zero: a zero would read as "free" in anything that printed
// it.
//
// It never returns an error. The table is belay's own knowledge, so a model
// it does not list may still be one the CLI runs, and refusing it here is
// what stopped belay starting on every model newer, older or more decorated
// than its table.
func (b *Backend) ListPrice(model string) (belay.ModelRate, error) {
	p, known := resolvePrice(b.prices(), model)
	if !known {
		warnUnpriced(b.logger(), model, p)
	}
	return belay.ModelRate{Input: p.Input, Output: p.Output}, nil
}
