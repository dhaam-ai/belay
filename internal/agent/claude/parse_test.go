//go:build unix

package claude

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		file          string
		wantSession   string
		wantTurns     int
		wantTextHas   string
		wantCostSet   bool
		wantCost      float64
		wantPerModel  string
		wantInputToks int64
	}{
		{
			name:          "success with cost fields",
			file:          "success_with_cost.json",
			wantSession:   "3f9c1b52-8a4d-4e21-9c77-2b6f0d5a1e83",
			wantTurns:     7,
			wantTextHas:   "Added the Login handler",
			wantCostSet:   true,
			wantCost:      0.4213,
			wantPerModel:  "claude-opus-5",
			wantInputToks: 12840,
		},
		{
			name:          "success without cost fields",
			file:          "success_without_cost.json",
			wantSession:   "7c2e5a90-1f34-4b8e-a6d2-9e0f3c7b45a1",
			wantTurns:     3,
			wantTextHas:   "Renamed the helper",
			wantCostSet:   false,
			wantPerModel:  "",
			wantInputToks: 40000,
		},
		{
			name:          "success carrying unknown fields",
			file:          "success_unknown_fields.json",
			wantSession:   "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9",
			wantTurns:     2,
			wantTextHas:   "Updated the README",
			wantCostSet:   true,
			wantCost:      0.0125,
			wantPerModel:  "claude-sonnet-5",
			wantInputToks: 1500,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := fixture(t, tt.file)

			res, raw, err := parseResult(body, false)
			if err != nil {
				t.Fatalf("parseResult: %v", err)
			}
			if res.SessionID != tt.wantSession {
				t.Errorf("SessionID = %q, want %q", res.SessionID, tt.wantSession)
			}
			if res.NumTurns != tt.wantTurns {
				t.Errorf("NumTurns = %d, want %d", res.NumTurns, tt.wantTurns)
			}
			if !strings.Contains(string(res.Result), tt.wantTextHas) {
				t.Errorf("Result = %q, want it to contain %q", res.Result, tt.wantTextHas)
			}
			if res.IsError {
				t.Error("IsError = true, want false")
			}
			if res.TotalCostUSD.Set != tt.wantCostSet {
				t.Errorf("TotalCostUSD.Set = %v, want %v", res.TotalCostUSD.Set, tt.wantCostSet)
			}
			if tt.wantCostSet && !almostEqual(res.TotalCostUSD.Value, tt.wantCost) {
				t.Errorf("TotalCostUSD.Value = %v, want %v", res.TotalCostUSD.Value, tt.wantCost)
			}
			if res.Usage.InputTokens != tt.wantInputToks {
				t.Errorf("Usage.InputTokens = %d, want %d", res.Usage.InputTokens, tt.wantInputToks)
			}
			if tt.wantPerModel != "" {
				per := res.perModel()
				if _, ok := per[tt.wantPerModel]; !ok {
					t.Errorf("perModel() = %v, want a %q entry", per, tt.wantPerModel)
				}
			} else if len(res.perModel()) != 0 {
				t.Errorf("perModel() = %v, want empty", res.perModel())
			}

			// Raw must be the bytes that arrived, not a re-encoding.
			if string(raw) != strings.TrimSpace(body) {
				t.Error("Raw is not the original body verbatim")
			}
			if !json.Valid(raw) {
				t.Error("Raw is not valid JSON")
			}
		})
	}
}

// TestParseIgnoresUnknownFieldsAndKeepsRaw is the acceptance test for
// version-tolerant parsing: a fixture full of fields this build has never
// heard of must parse cleanly, and everything it dropped must still be
// recoverable from Raw.
func TestParseIgnoresUnknownFieldsAndKeepsRaw(t *testing.T) {
	t.Parallel()
	body := fixture(t, "success_unknown_fields.json")

	res, raw, err := parseResult(body, false)
	if err != nil {
		t.Fatalf("a fixture with unknown fields must parse, got: %v", err)
	}
	if res.SessionID == "" || res.Result == "" {
		t.Fatal("known fields were not decoded alongside the unknown ones")
	}

	// Everything the struct dropped is still in Raw.
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("Raw did not round-trip: %v", err)
	}
	for _, key := range []string{
		"future_top_level_object", "future_top_level_scalar",
		"structured_output", "uuid", "permission_denials", "duration_api_ms",
	} {
		if _, ok := round[key]; !ok {
			t.Errorf("Raw lost the %q field", key)
		}
	}
	nested, ok := round["usage"].(map[string]any)
	if !ok {
		t.Fatal("Raw lost the usage object")
	}
	if _, ok := nested["future_token_bucket"]; !ok {
		t.Error("Raw lost an unknown field nested inside usage")
	}
}

func TestParseRejectsUnusableBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stdout    string
		truncated bool
		wantIn    string
	}{
		{
			name:   "truncated mid string",
			stdout: fixture(t, "malformed_truncated.json"),
			wantIn: "unexpected EOF",
		},
		{
			name:      "truncated and flagged by the capture",
			stdout:    fixture(t, "malformed_truncated.json"),
			truncated: true,
			wantIn:    "truncated at the capture limit",
		},
		{name: "empty stdout", stdout: "", wantIn: "no output on stdout"},
		{name: "whitespace only", stdout: "   \n\t ", wantIn: "no output on stdout"},
		{
			name:   "not JSON at all",
			stdout: "<html><body>502 Bad Gateway</body></html>",
			wantIn: "invalid character",
		},
		{
			name:   "stream-json reached the single-object parser",
			stdout: `{"type":"system"}` + "\n" + `{"type":"result","result":"hi"}`,
			wantIn: "trailing content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, raw, err := parseResult(tt.stdout, tt.truncated)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !errors.Is(err, ErrUnparsableOutput) {
				t.Errorf("errors.Is(err, ErrUnparsableOutput) = false for %v", err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("errors.As(*ParseError) = false for %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q, want it to mention %q", err, tt.wantIn)
			}
			if raw != nil {
				t.Error("a failed parse must not return a Raw body")
			}
		})
	}
}

// TestFlexFloatNeverFailsAParse pins the requirement that an unreadable
// total_cost_usd degrades to "absent" rather than killing the whole decode.
func TestFlexFloatNeverFailsAParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantSet bool
		want    float64
	}{
		{name: "number", body: `{"total_cost_usd":0.42}`, wantSet: true, want: 0.42},
		{name: "integer", body: `{"total_cost_usd":3}`, wantSet: true, want: 3},
		{name: "zero", body: `{"total_cost_usd":0}`, wantSet: true, want: 0},
		{name: "numeric string", body: `{"total_cost_usd":"0.42"}`, wantSet: true, want: 0.42},
		{name: "absent", body: `{}`, wantSet: false},
		{name: "null", body: `{"total_cost_usd":null}`, wantSet: false},
		{name: "non numeric string", body: `{"total_cost_usd":"free"}`, wantSet: false},
		{name: "object", body: `{"total_cost_usd":{"amount":1}}`, wantSet: false},
		{name: "array", body: `{"total_cost_usd":[1,2]}`, wantSet: false},
		{name: "bool", body: `{"total_cost_usd":true}`, wantSet: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, _, err := parseResult(tt.body, false)
			if err != nil {
				t.Fatalf("an unreadable cost must not fail the parse, got: %v", err)
			}
			if res.TotalCostUSD.Set != tt.wantSet {
				t.Fatalf("Set = %v, want %v", res.TotalCostUSD.Set, tt.wantSet)
			}
			if tt.wantSet && !almostEqual(res.TotalCostUSD.Value, tt.want) {
				t.Errorf("Value = %v, want %v", res.TotalCostUSD.Value, tt.want)
			}
		})
	}
}

// TestFlexStringSurvivesATypeChange covers the SDK typing `result` as
// `unknown`: a future version returning something other than a string must
// still yield usable text.
func TestFlexStringSurvivesATypeChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "string", body: `{"result":"done"}`, want: "done"},
		{name: "empty string", body: `{"result":""}`, want: ""},
		{name: "object", body: `{"result":{"text":"done"}}`, want: `{"text":"done"}`},
		{name: "number", body: `{"result":42}`, want: "42"},
		// null decodes to the empty string, which is the useful reading:
		// "the CLI returned no text", not the literal word null.
		{name: "null", body: `{"result":null}`, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, _, err := parseResult(tt.body, false)
			if err != nil {
				t.Fatalf("parseResult: %v", err)
			}
			if got := string(res.Result); got != tt.want {
				t.Errorf("Result = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseAcceptsBothModelUsageSpellings covers the camelCase the CLI emits
// and the snake_case the Python SDK renames it to.
func TestParseAcceptsBothModelUsageSpellings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "camelCase", body: `{"modelUsage":{"claude-opus-5":{"inputTokens":10}}}`},
		{name: "snake_case", body: `{"model_usage":{"claude-opus-5":{"inputTokens":10}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, _, err := parseResult(tt.body, false)
			if err != nil {
				t.Fatalf("parseResult: %v", err)
			}
			per := res.perModel()
			if got := per["claude-opus-5"].InputTokens; got != 10 {
				t.Errorf("perModel()[claude-opus-5].InputTokens = %d, want 10", got)
			}
		})
	}
}

func TestSnippetStaysPrintable(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", snippetLen)
	got := snippet(long)
	if len(got) > snippetLen+len("...") {
		t.Errorf("snippet len = %d, want <= %d", len(got), snippetLen+3)
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("a truncated snippet should be marked with an ellipsis")
	}
	if strings.ContainsRune(strings.TrimSuffix(got, "..."), '�') {
		t.Error("snippet split a multi-byte rune")
	}
	if short := snippet("abc"); short != "abc" {
		t.Errorf("snippet(%q) = %q, want it unchanged", "abc", short)
	}
}
