// Command belay is the CLI entry point for the belay agentic engineering graph.
package main

import (
	"log"

	"github.com/dhaam-ai/belay/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		log.Fatal(err)
	}
}
