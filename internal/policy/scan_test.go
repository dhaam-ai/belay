package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dhaam-ai/belay/internal/policy"
)

// A minified bundle or a generated blob has lines longer than anything belay
// will hold in memory at once, and a content rule cannot be honestly applied
// to a line it only read part of. "belay could not look" must never be
// recorded as "belay looked and it was fine", so the gate refuses and the
// message names the file and both ways out of it.
//
// Line counting is not affected: it needs no content, so a policy that only
// bounds size still judges such a file.
func TestAnOverLongLineRefusesAContentRuleButStillCounts(t *testing.T) {
	t.Parallel()

	const lineBytes = 1<<20 + 16 // just over the cap the scanner holds
	files := map[string]string{
		"bundle.min.js": strings.Repeat("x", lineBytes) + "\n",
		"a.go":          "package a\n",
	}
	changed := []string{"a.go", "bundle.min.js"}

	_, err := policy.Evaluate(context.Background(),
		policy.Policy{ForbiddenContent: []policy.ContentRule{{Pattern: "secret"}}},
		policy.Baseline{}, policy.Change{Workspace: tree(t, files), Files: changed})
	if err == nil {
		t.Fatal("Evaluate returned no error for a file it could not scan; a skipped check must never read as a pass")
	}
	for _, fragment := range []string{"bundle.min.js", "forbidden_content", "forbidden_paths"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Evaluate error = %v; it must name %q so a person can act on it", err, fragment)
		}
	}

	// The same file under a rule that needs no content is judged normally.
	report, err := policy.Evaluate(context.Background(),
		policy.Policy{MaxChangedLines: 1},
		policy.Baseline{}, policy.Change{Workspace: tree(t, files), Files: changed})
	if err != nil {
		t.Fatalf("Evaluate with only a size ceiling: %v", err)
	}
	if diff := rules(report); len(diff) != 1 || diff[0] != string(policy.RuleMaxChangedLines) {
		t.Errorf("violations = %v, want exactly the line ceiling", diff)
	}
}

// A changed path that is not a regular file is not something to read to the
// end of, and not something a content rule can be about. A deletion is the
// ordinary case and contributes nothing rather than stopping the gate.
func TestAPathThatIsNotAFileIsNotScanned(t *testing.T) {
	t.Parallel()

	dir := tree(t, map[string]string{"pkg/a.go": "package a\n"})
	report, err := policy.Evaluate(context.Background(),
		policy.Policy{MaxChangedLines: 5, ForbiddenContent: []policy.ContentRule{{Pattern: "package"}}},
		policy.Baseline{},
		// "pkg" is a directory and "gone.go" was deleted; neither is a file
		// with lines or text in it.
		policy.Change{Workspace: dir, Files: []string{"pkg", "gone.go"}})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !report.Clean() {
		t.Errorf("violations = %+v, want none: neither path is a file to read", report.Violations)
	}
}
