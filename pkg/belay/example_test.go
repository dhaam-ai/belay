package belay_test

import (
	"context"
	"fmt"

	"github.com/belay-dev/belay/pkg/belay"
)

// echoBackend is a minimal third-party belay.AgentBackend. It does no real
// agentic work — it just echoes the prompt back — to demonstrate the shape
// a real integration (shelling out to a CLI, calling an HTTP API) fills in.
// See doc.go's "Extension model" section for a fuller, CLI-backed sketch.
type echoBackend struct{}

// Name implements belay.AgentBackend.
func (echoBackend) Name() string { return "echo" }

// Invoke implements belay.AgentBackend.
func (echoBackend) Invoke(ctx context.Context, req belay.AgentRequest) (belay.AgentResponse, error) {
	if err := ctx.Err(); err != nil {
		return belay.AgentResponse{}, err
	}
	return belay.AgentResponse{
		Text:      "echo: " + req.Prompt,
		SessionID: req.SessionID,
		Usage:     belay.Usage{Estimated: true},
	}, nil
}

// compile-time proof that echoBackend needs nothing beyond the interface.
var _ belay.AgentBackend = echoBackend{}

// Example implements a third-party AgentBackend and drives it through the
// same call the graph runner makes.
func Example() {
	var backend belay.AgentBackend = echoBackend{}

	resp, err := backend.Invoke(context.Background(), belay.AgentRequest{
		Prompt:  "implement the login handler",
		WorkDir: "/tmp/workspace",
	})
	if err != nil {
		fmt.Println("invoke failed:", err)
		return
	}
	fmt.Println(resp.Text)
	// Output:
	// echo: implement the login handler
}
