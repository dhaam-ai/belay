package belay_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/pkg/belay"
)

func TestAgentRequestJSONRoundTrip(t *testing.T) {
	t.Parallel()

	tests := map[string]belay.AgentRequest{
		"zero value": {},
		"fully populated": {
			Prompt:       "implement the login handler",
			SystemPrompt: "you are a careful senior engineer",
			WorkDir:      "/work/repo",
			AllowedTools: []string{"Read", "Write", "Bash"},
			MaxTurns:     12,
			Model:        "claude-opus-4",
			SessionID:    "sess-123",
			MCPConfig:    json.RawMessage(`{"servers":{"sonar":{"url":"http://localhost:9000"}}}`),
		},
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}

			var got belay.AgentRequest
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
			}
			if diff := cmp.Diff(want, got, rawMessageComparer); diff != "" {
				t.Errorf("round trip through %s changed the value (-want +got):\n%s", encoded, diff)
			}
		})
	}
}

func TestAgentResponseJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := belay.AgentResponse{
		Text:      "done",
		SessionID: "sess-123",
		Usage: belay.Usage{
			InputTokens:  100,
			OutputTokens: 42,
			USD:          0.0123,
			Estimated:    true,
		},
		Turns: 3,
		Raw:   json.RawMessage(`{"total_cost_usd":0.0123}`),
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var got belay.AgentResponse
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	if diff := cmp.Diff(want, got, rawMessageComparer); diff != "" {
		t.Errorf("round trip through %s changed the value (-want +got):\n%s", encoded, diff)
	}
}

// TestUsageJSONFieldNames pins Usage's wire format. Usage is read by budget
// guards that may live in a different process or a different language
// entirely (a journal viewer, a dashboard); renaming a Go field here is a
// breaking change to that wire format even though the Go type itself would
// still compile for every caller, which is exactly the kind of change
// go vet cannot catch and this test exists to.
func TestUsageJSONFieldNames(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(belay.Usage{InputTokens: 1, OutputTokens: 2, USD: 3, Estimated: true})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}

	for _, field := range []string{"input_tokens", "output_tokens", "usd", "estimated"} {
		if _, ok := asMap[field]; !ok {
			t.Errorf("Usage JSON %s is missing expected key %q", encoded, field)
		}
	}
	if len(asMap) != 4 {
		t.Errorf("Usage JSON %s has %d keys, want exactly 4", encoded, len(asMap))
	}
}
