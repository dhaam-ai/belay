//go:build fixrate

package fixrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// postRunTestTimeout bounds the harness's own re-run of `go test -race
// -json ./...` against the workspace once belay's dispatcher has
// finished. It is independent of, and does not replace, the test node's
// own timeout (config.Graph.NodeTimeout) during the run itself.
const postRunTestTimeout = 3 * time.Minute

// testEvent is one line of `go test -json`'s event stream — only the
// fields this package reads.
type testEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

// leafTestResults runs `go test -race -json ./...` in dir and returns the
// final action ("pass", "fail" or "skip") for every LEAF test name it
// reported a result for — a test with no subtest of its own. A name
// absent from the result did not run to completion, including because a
// panic in an earlier test aborted the whole binary before it started.
//
// This mirrors fixtures/seeded-bug/inject.go's own runGoTestJSON +
// leafResults exactly (down to the -race flag), by design and necessity:
// Run's diff compares this package's post-run observation against
// injected.json's pre-run one, and the two are only comparable if they
// are produced the same way. It is not imported from inject.go because
// inject.go is a single-file "package main" in a separate module (see
// doc.go) with nothing exported to import in the first place.
func leafTestResults(ctx context.Context, dir string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, postRunTestTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-json", "./...") //nolint:gosec // fixed argv; dir is this harness's own temp workspace
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	all := make(map[string]string)
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var ev testEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			all[ev.Test] = ev.Action
		}
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return all, fmt.Errorf("fixrate: running go test in %s: %w (stderr: %s)", dir, runErr, stderr.String())
		}
		// A non-zero exit from failing tests (or a panic) is expected and
		// already reflected in all; nothing more to do.
	}

	return leaves(all), nil
}

// leaves returns the subset of results whose name is not a t.Run ancestor
// of any other name present — see fixtures/seeded-bug/inject.go's
// leafResults for the identical rationale: a parent test inherits its
// subtests' status purely through go test's own aggregation, so keeping
// only leaves avoids reporting one underlying failure twice under two
// names.
func leaves(results map[string]string) map[string]string {
	hasChild := make(map[string]bool, len(results))
	for a := range results {
		for b := range results {
			if a != b && strings.HasPrefix(b, a+"/") {
				hasChild[a] = true
				break
			}
		}
	}
	out := make(map[string]string, len(results))
	for name, action := range results {
		if !hasChild[name] {
			out[name] = action
		}
	}
	return out
}

// testStatus is the outcome of one catalogued test name against a
// leafTestResults map.
type testStatus string

const (
	statusPass   testStatus = "pass"
	statusFail   testStatus = "fail"
	statusAbsent testStatus = "absent" // no result at all: never ran (deleted, or a panic upstream aborted the binary)
)

func statusOf(results map[string]string, name string) testStatus {
	switch results[name] {
	case "pass":
		return statusPass
	case "fail":
		return statusFail
	default:
		return statusAbsent
	}
}
