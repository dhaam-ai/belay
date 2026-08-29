#!/bin/sh
# Illustrative example of the naive one-shot approach.
# NOT production code. This example intentionally lacks:
# - Error recovery
# - State persistence
# - Retry logic
# - Budget tracking
# - Output validation
# - Human approval gates
#
# It demonstrates why belay exists: process dies mid-run,
# you restart from zero with no visibility into what happened or how much it cost.

set -eu

# Refuse to run if claude is not on PATH
if ! command -v claude >/dev/null 2>&1; then
    echo "Error: 'claude' command not found on PATH"
    echo "Install the claude CLI or ensure it is in your PATH"
    exit 1
fi

# Refuse to run if jq is not available (needed to parse the output)
if ! command -v jq >/dev/null 2>&1; then
    echo "Error: 'jq' command not found on PATH"
    echo "Install jq to parse the JSON output"
    exit 1
fi

PROMPT="Write a function in Go that validates an email address. Include a docstring explaining the logic."

echo "Invoking claude with one-shot prompt..."
echo "Prompt: $PROMPT"
echo ""

# One agent call. One output file. No retry, no state, no recovery.
# If this process dies, all work is lost and we restart from zero.
claude -p "$PROMPT" --output-format json > out.json

# Parse the result
TOTAL_COST=$(jq -r '.total_cost_usd' out.json)
NUM_TURNS=$(jq -r '.num_turns' out.json)
DURATION_MS=$(jq -r '.duration_ms' out.json)
SESSION_ID=$(jq -r '.session_id' out.json)

echo "Done."
echo "Session ID: $SESSION_ID"
echo "Turns: $NUM_TURNS"
echo "Duration: ${DURATION_MS}ms"
echo "Cost: \$$TOTAL_COST"
echo ""
echo "Full output written to out.json"
