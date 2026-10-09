//go:build unix

package claude

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/budget"
	"github.com/dhaam-ai/belay/internal/config"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// TestCostScenarios is the acceptance test for ADR-0007's degradation rule:
// the budget guard must always be handed a usable number or a loud error, and
// must never be handed a silent zero.
//
// Each case names which of the four documented outcomes it pins.
func TestCostScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		fixture       string // one of fixture or body is set
		body          string
		model         string
		prices        map[string]Price
		wantUSD       float64
		wantEstimated bool
		wantInput     int64
		wantOutput    int64
		// wantUnpriced is every model usageOf had to charge at the highest
		// rate, which the backend warns about.
		wantUnpriced []string
	}{
		{
			// 1. Cost fields present: report them verbatim, exactly.
			name:    "reported cost is used verbatim and is not estimated",
			fixture: "success_with_cost.json",
			model:   "claude-opus-5",
			wantUSD: 0.4213,
			// 12840 input + 8200 cache write + 154300 cache read.
			wantInput:     175340,
			wantOutput:    3120,
			wantEstimated: false,
		},
		{
			// 2. Cost fields absent: estimate, non-zero, flagged.
			// 40000 in * $2/MTok + 10000 out * $10/MTok = 0.08 + 0.10.
			name:          "absent cost falls back to the price table",
			fixture:       "success_without_cost.json",
			model:         "claude-sonnet-5",
			wantUSD:       0.18,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// 2b. The bare alias belay's own config defaults to.
			name:          "the sonnet alias prices like claude-sonnet-5",
			fixture:       "success_without_cost.json",
			model:         "sonnet",
			wantUSD:       0.18,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// 3. Unknown model and nothing reported: charged at the highest
			// rate belay knows ($10/$50), flagged, and named. It used to be
			// a *PriceError, which stopped belay starting at all on any
			// model newer or older than its table.
			// 40000 * $10/MTok + 10000 * $50/MTok = 0.40 + 0.50.
			name:          "an unknown model is charged the highest known rate, never a zero",
			fixture:       "success_without_cost.json",
			model:         "claude-unobtainium-9",
			wantUSD:       0.90,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
			wantUnpriced:  []string{"claude-unobtainium-9"},
		},
		{
			// Alias shapes the CLI accepts resolve to the entry they name.
			name:          "a context-window alias prices like its family",
			fixture:       "success_without_cost.json",
			model:         "sonnet[1m]",
			wantUSD:       0.18,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// opusplan is Opus while planning and Sonnet otherwise; the
			// dearer of the two is the one a ceiling can rely on.
			// 40000 * $5/MTok + 10000 * $25/MTok = 0.20 + 0.25.
			name:          "opusplan prices as opus",
			fixture:       "success_without_cost.json",
			model:         "opusplan",
			wantUSD:       0.45,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// 40000 * $1/MTok + 10000 * $5/MTok = 0.04 + 0.05.
			name:          "a dated identifier prices like the model it dates",
			fixture:       "success_without_cost.json",
			model:         "claude-haiku-4-5-20251001",
			wantUSD:       0.09,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// A real model belay's table does not list is not guessed from
			// its family, whose members can differ threefold in price.
			name:          "a model outside the table is not priced from its family",
			fixture:       "success_without_cost.json",
			model:         "claude-opus-4-1",
			wantUSD:       0.90,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
			wantUnpriced:  []string{"claude-opus-4-1"},
		},
		{
			// 4. Unknown fields alongside a real cost: still exact.
			name:          "unknown fields do not disturb the reported cost",
			fixture:       "success_unknown_fields.json",
			model:         "claude-sonnet-5",
			wantUSD:       0.0125,
			wantInput:     1500,
			wantOutput:    300,
			wantEstimated: false,
		},
		{
			// The session-crash shape: the CLI zeroes total_cost_usd but
			// leaves the token counts intact. Trusting the zero would lose
			// the whole node from the ledger.
			name: "a zeroed cost with tokens spent is estimated, not trusted",
			body: `{"total_cost_usd":0,"usage":{"input_tokens":40000,` +
				`"output_tokens":10000}}`,
			model:         "claude-sonnet-5",
			wantUSD:       0.18,
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
		{
			// Nothing reported and nothing spent: zero is the honest
			// answer, but it is still flagged, because it was not confirmed.
			name:          "no cost and no tokens is a flagged zero",
			body:          `{"result":"nothing to do"}`,
			model:         "claude-unobtainium-9",
			wantUSD:       0,
			wantEstimated: true,
		},
		{
			// modelUsage is preferred over usage because it is the only
			// field that counts subagent spend. Here usage reports the main
			// loop only and modelUsage reports both models.
			name: "per-model usage is preferred and sums across models",
			body: `{"usage":{"input_tokens":1000,"output_tokens":100},` +
				`"modelUsage":{` +
				`"claude-opus-5":{"inputTokens":1000,"outputTokens":100},` +
				`"claude-haiku-4-5":{"inputTokens":2000,"outputTokens":200}}}`,
			model: "claude-opus-5",
			// opus: 1000*5e-6 + 100*25e-6 = 0.005 + 0.0025 = 0.0075
			// haiku: 2000*1e-6 + 200*5e-6 = 0.002 + 0.001   = 0.003
			wantUSD:       0.0105,
			wantInput:     3000,
			wantOutput:    300,
			wantEstimated: true,
		},
		{
			// Cache tiers are priced at their published multipliers, not at
			// the base input rate: pricing 100k cache reads as full input
			// would overstate this call roughly fourfold.
			name: "cache tiers are priced at their own multipliers",
			body: `{"usage":{"input_tokens":1000,"output_tokens":500,` +
				`"cache_creation_input_tokens":2000,` +
				`"cache_read_input_tokens":100000}}`,
			model: "claude-opus-5",
			// 1000*5e-6            = 0.0050
			// 2000*5e-6*1.25       = 0.0125
			// 100000*5e-6*0.10     = 0.0500
			// 500*25e-6            = 0.0125
			wantUSD:       0.08,
			wantInput:     103000,
			wantOutput:    500,
			wantEstimated: true,
		},
		{
			name:          "a custom price table overrides the built-in one",
			fixture:       "success_without_cost.json",
			model:         "house-model",
			prices:        map[string]Price{"house-model": {Input: 1, Output: 1}},
			wantUSD:       0.05, // 40000*1e-6 + 10000*1e-6
			wantInput:     40000,
			wantOutput:    10000,
			wantEstimated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := tt.body
			if tt.fixture != "" {
				body = fixture(t, tt.fixture)
			}
			parsed, _, err := parseResult(body, false)
			if err != nil {
				t.Fatalf("parseResult: %v", err)
			}

			table := tt.prices
			if table == nil {
				table = defaultPrices
			}

			got, unpriced := usageOf(parsed, tt.model, table)
			if !slices.Equal(unpriced, tt.wantUnpriced) {
				t.Errorf("unpriced models = %v, want %v", unpriced, tt.wantUnpriced)
			}
			if !almostEqual(got.USD, tt.wantUSD) {
				t.Errorf("USD = %v, want %v", got.USD, tt.wantUSD)
			}
			if got.Estimated != tt.wantEstimated {
				t.Errorf("Estimated = %v, want %v", got.Estimated, tt.wantEstimated)
			}
			if got.InputTokens != tt.wantInput {
				t.Errorf("InputTokens = %d, want %d", got.InputTokens, tt.wantInput)
			}
			if got.OutputTokens != tt.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", got.OutputTokens, tt.wantOutput)
			}
			// The guard's core invariant: tokens were spent, so the ledger
			// must never be handed nothing.
			if tt.wantInput+tt.wantOutput > 0 && got.USD <= 0 {
				t.Errorf("USD = %v for a call that spent tokens; a silent zero defeats the budget guard", got.USD)
			}
		})
	}
}

func TestLookupPrice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		want  Price
		found bool
	}{
		{name: "exact id", model: "claude-opus-5", want: Price{5, 25}, found: true},
		{name: "alias", model: "sonnet", want: Price{2, 10}, found: true},
		{name: "bedrock prefix", model: "anthropic.claude-opus-5", want: Price{5, 25}, found: true},
		{name: "region and vendor prefix", model: "us.anthropic.claude-sonnet-5", want: Price{2, 10}, found: true},
		{name: "vertex dated snapshot", model: "claude-opus-5@20260101", want: Price{5, 25}, found: true},
		{name: "mixed case", model: "Claude-Opus-5", want: Price{5, 25}, found: true},
		{name: "surrounding space", model: "  claude-haiku-4-5 ", want: Price{1, 5}, found: true},
		{name: "context-window alias", model: "sonnet[1m]", want: Price{2, 10}, found: true},
		{name: "plan-mode alias", model: "opusplan", want: Price{5, 25}, found: true},
		{name: "dated identifier", model: "claude-haiku-4-5-20251001", want: Price{1, 5}, found: true},
		{name: "bedrock versioned identifier", model: "anthropic.claude-haiku-4-5-20251001-v1:0", want: Price{1, 5}, found: true},
		{name: "unknown model", model: "claude-unobtainium-9"},
		{name: "empty model", model: ""},
		// Guessing by prefix would be a confident wrong answer; a miss is
		// the intended outcome for a model nobody has priced yet.
		{name: "future model is not guessed from a prefix", model: "claude-opus-6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := lookupPrice(defaultPrices, tt.model)
			if ok != tt.found {
				t.Fatalf("lookupPrice(%q) found = %v, want %v", tt.model, ok, tt.found)
			}
			if tt.found && got != tt.want {
				t.Errorf("lookupPrice(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
		})
	}
}

func TestNormalizeModel(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"claude-opus-5", "claude-opus-5"},
		{"anthropic.claude-opus-5", "claude-opus-5"},
		{"us.anthropic.claude-opus-5", "claude-opus-5"},
		{"claude-opus-4-5@20251101", "claude-opus-4-5"},
		{"  Claude-Sonnet-5  ", "claude-sonnet-5"},
		{"some.other.thing", "some.other.thing"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := normalizeModel(tt.in); got != tt.want {
				t.Errorf("normalizeModel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestDefaultPricesIsACopy guards the built-in table against a caller that
// mutates what it was handed.
func TestDefaultPricesIsACopy(t *testing.T) {
	t.Parallel()

	got := DefaultPrices()
	if len(got) != len(defaultPrices) {
		t.Fatalf("DefaultPrices() has %d entries, want %d", len(got), len(defaultPrices))
	}
	got["claude-opus-5"] = Price{Input: 999, Output: 999}
	delete(got, "sonnet")

	if p := defaultPrices["claude-opus-5"]; p != (Price{Input: 5, Output: 25}) {
		t.Errorf("the built-in table was mutated: claude-opus-5 = %+v", p)
	}
	if _, ok := defaultPrices["sonnet"]; !ok {
		t.Error("the built-in table lost an entry to a caller's delete")
	}
}

// TestEveryPriceIsPositive catches a typo that would silently make a model
// free, which the budget guard would never notice.
func TestEveryPriceIsPositive(t *testing.T) {
	t.Parallel()

	for model, p := range defaultPrices {
		if p.Input <= 0 {
			t.Errorf("%s: Input = %v, want > 0", model, p.Input)
		}
		if p.Output <= 0 {
			t.Errorf("%s: Output = %v, want > 0", model, p.Output)
		}
		if p.Output < p.Input {
			t.Errorf("%s: Output %v < Input %v, which no Claude model prices at",
				model, p.Output, p.Input)
		}
	}
}

// A model is the backend's business: only whatever will run the request knows
// which names it accepts and what they cost. The CLI asks this before a run
// starts, for every node's model and not just agent.model, so a name that
// cannot be run or priced stops the run before it spends rather than in the
// middle of it.
func TestListPriceAnswersForAModelTheBackendCanRun(t *testing.T) {
	t.Parallel()
	b := New(nil)

	tests := []struct {
		name            string
		model           string
		wantIn, wantOut float64
	}{
		{"an alias the CLI accepts", "haiku", 1, 5},
		{"a full identifier", "claude-sonnet-5", 2, 10},
		{"belay's own default", "sonnet", 2, 10},
		{"a dated snapshot normalises", "claude-opus-5@20251101", 5, 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate, err := b.ListPrice(tt.model)
			if err != nil {
				t.Fatalf("ListPrice(%q) = %v", tt.model, err)
			}
			if rate.Input != tt.wantIn || rate.Output != tt.wantOut {
				t.Errorf("ListPrice(%q) = %v per million, want input %v output %v",
					tt.model, rate, tt.wantIn, tt.wantOut)
			}
			if rate.Assumed {
				t.Errorf("ListPrice(%q).Assumed = true, want false: belay prices this model from its own table", tt.model)
			}
		})
	}
}

// A model outside belay's table is not a refusal. belay refused to start on
// any model its table did not list, which an upgrade of the CLI, a dated
// identifier or an alias such as sonnet[1m] or opusplan was enough to cause.
// The rate is the highest belay knows, never a zero, so the dry run never
// shows a model as free and the ceiling is held at the safe end; and belay
// says so, once per model, naming it.
func TestListPriceChargesAModelOutsideTheTableTheHighestKnownRate(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	b := &Backend{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	model := "claude-opus-9-" + t.Name()

	for range 2 {
		rate, err := b.ListPrice(model)
		if err != nil {
			t.Fatalf("ListPrice(%q) = %v, want the highest known rate", model, err)
		}
		if rate.Input != 10 || rate.Output != 50 {
			t.Errorf("ListPrice(%q) = %+v, want the highest known rate, $10 in and $50 out", model, rate)
		}
		if !rate.Assumed {
			t.Errorf("ListPrice(%q).Assumed = false, want true: the rate is a stand-in, and a caller "+
				"reporting what something cost must be able to tell", model)
		}
	}
	if n := strings.Count(logs.String(), "most expensive rate"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), model) {
		t.Errorf("the warning does not name the model:\n%s", logs.String())
	}
}

// TestTheCeilingHoldsForAModelOutsideTheTable: the reason belay refused an
// unpriceable model was that a ceiling it could not price was no ceiling. A
// call on such a model whose CLI reports no cost is charged at the highest
// known rate, so the ledger still sees money and the guard still stops the
// run.
func TestTheCeilingHoldsForAModelOutsideTheTable(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{Result: okResult(fixture(t, "success_without_cost.json"))}
	b := newBackend(t, r)
	req := validRequest()
	req.Model = "claude-sonnet-4-5"

	resp, err := b.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke() error = %v, want a response priced at the highest known rate", err)
	}
	if !resp.Usage.Estimated || !almostEqual(resp.Usage.USD, 0.90) {
		t.Fatalf("Usage = %+v, want $0.90 marked estimated", resp.Usage)
	}
	ledger := budget.New()
	ledger.Record("code", resp.Usage)
	if err := ledger.Check(config.Budget{MaxUSD: 0.50, OnExceed: config.OnExceedAbort}); !errors.Is(err, belay.ErrBudgetExceeded) {
		t.Errorf("Check() = %v after a $0.90 call against $0.50, want the ceiling to hold", err)
	}
}

// The backend belay wires in must be the one it asks about models, or the
// dry run prices a table the run does not use.
var _ belay.ModelPricer = (*Backend)(nil)
