package plandoc_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/dhaam-ai/belay/internal/nodes/approve"
	"github.com/dhaam-ai/belay/internal/nodes/plan"
	"github.com/dhaam-ai/belay/internal/plandoc"
)

// TestTheHarnessPlanReadsAsOneStep grounds the format in a plan belay really
// produces rather than one this package invented.
//
// testdata/harness.md is byte for byte the Text of loopPlanResponse in
// test/e2e/loop_agent.go — the plan the end-to-end harness's agent returns —
// archived the way plan.Node.Run archives one: trimmed, with a single
// trailing newline. Its sections follow prompt.go directly, and its "##
// Steps" heading is followed by the list with no blank line between them.
func TestTheHarnessPlanReadsAsOneStep(t *testing.T) {
	doc, _ := parse(t, "harness.md")

	want := []plandoc.Step{{Number: 1, Text: "Add an `Add(a, b int) int` function to src/app.go."}}
	if diff := cmp.Diff(want, doc.Steps()); diff != "" {
		t.Errorf("Steps() mismatch (-want +got):\n%s", diff)
	}
}

// TestOpeningAPlanNeverMovesTheGatesDigest is the compatibility test, stated
// in the gate's own terms.
//
// The approve gate re-fingerprints plan.md every time it runs, records the
// new digest, and says "The plan was edited" when it moved. So the cost of
// this package getting losslessness wrong is not cosmetic: a person who
// opened their plan, scrolled it and saved would be recorded as having
// edited it, and the digest the run was approved under would no longer be
// the digest of the plan the agent wrote.
//
// Both digest functions are the real ones. plan.Digest writes state.Plan's
// fingerprint and approve.Digest re-checks it; this package must be
// invisible to both.
func TestOpeningAPlanNeverMovesTheGatesDigest(t *testing.T) {
	for _, fixture := range parseable {
		t.Run(fixture, func(t *testing.T) {
			doc, src := parse(t, fixture)

			opened := map[string][]byte{"parsed": doc.Bytes()}
			for i, s := range doc.Steps() {
				saved, err := doc.Edit(i, s.Text)
				if err != nil {
					t.Fatalf("Edit(%d, its own text) = error %v", i, err)
				}
				opened["saved step "+s.Text[:min(len(s.Text), 12)]] = saved.Bytes()

				moved, err := doc.Move(i, i)
				if err != nil {
					t.Fatalf("Move(%d, %d) = error %v", i, i, err)
				}
				opened["step left where it is"] = moved.Bytes()
			}

			for what, got := range opened {
				if plan.Digest(got) != plan.Digest(src) {
					t.Errorf("%s: plan.Digest moved\n got: %s over %q\nwant: %s", what, plan.Digest(got), got, plan.Digest(src))
				}
				if approve.Digest(got) != approve.Digest(src) {
					t.Errorf("%s: approve.Digest moved\n got: %s over %q\nwant: %s", what, approve.Digest(got), got, approve.Digest(src))
				}
			}
		})
	}
}

// TestAHandEditedPlanIsCarriedThroughUntouched covers what people actually
// do to a plan while a run waits at the approve gate, in the editor of their
// choice: renumber by hand after deleting a step, wrap a line without
// indenting it, leave trailing spaces, save with CR LF, replace the list
// with bullets or sub-headings, add a section, paste in code.
//
// Every one of them must keep working exactly as it does today. Today the
// gate reads the bytes on disk and fingerprints them, and nothing about a
// plan's shape makes it unapprovable. So for each shape there are only two
// allowed outcomes: this package reads it and hands the same bytes back, or
// it refuses with one of its documented errors — in which case the caller
// never got a document and still holds the file exactly as the person left
// it. What is not allowed is reading the plan and handing back something
// different.
func TestAHandEditedPlanIsCarriedThroughUntouched(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		steps int   // how many steps the plan is read as
		want  error // nil when the plan is editable
	}{
		{
			name:  "a step deleted by hand, leaving a gap in the numbering",
			src:   "## Steps\n\n1. Read the manifest.\n3. Refuse a newer schema.\n\n## Tests\n\nNone yet.\n",
			steps: 2,
		},
		{
			name:  "a line wrapped without indenting it",
			src:   "## Steps\n\n1. Read the manifest, which holds the run's pinned wiring and\nthe capabilities it requires.\n2. Rebuild the registry.\n",
			steps: 1,
		},
		{
			name:  "trailing spaces left on the lines",
			src:   "## Steps   \n\n1. Read the manifest.   \n2. Rebuild the registry.  \n\n## Tests  \n\nNone.  \n",
			steps: 2,
		},
		{
			name:  "saved from a Windows editor",
			src:   "## Steps\r\n\r\n1. Read the manifest.\r\n2. Rebuild the registry.\r\n",
			steps: 2,
		},
		{
			name:  "a section added at the end",
			src:   "## Steps\n\n1. Read the manifest.\n\n## Out of scope\n\nNothing else.\n\n## Notes from the reviewer\n\nCheck the schema version first.\n",
			steps: 1,
		},
		{
			name:  "code pasted into a step",
			src:   "## Steps\n\n1. Add the helper:\n\n   ```go\n   func Add(a, b int) int { return a + b }\n   ```\n\n2. Cover it.\n",
			steps: 2,
		},
		{
			name:  "an emoji and an em dash typed into a step",
			src:   "## Steps\n\n1. Ship it 🚀 — carefully.\n2. Tell 田中 in #releases.\n",
			steps: 2,
		},
		{
			name:  "no newline at the end of the file",
			src:   "## Steps\n\n1. Read the manifest.\n2. Rebuild the registry.",
			steps: 2,
		},
		{
			name: "the whole plan rewritten as prose",
			src:  "## Summary\n\nJust do the obvious thing and stop.\n",
			want: plandoc.ErrNoSteps,
		},
		{
			name: "the steps rewritten as bullets",
			src:  "## Steps\n\n- Read the manifest.\n- Rebuild the registry.\n",
			want: plandoc.ErrNoSteps,
		},
		{
			name: "the steps rewritten as sub-headings",
			src:  "## Steps\n\n### Step 1\n\nRead the manifest.\n\n### Step 2\n\nRebuild the registry.\n",
			want: plandoc.ErrNoSteps,
		},
		{
			name: "saved in the wrong encoding",
			src:  "## Steps\n\n1. R\xe9sum\xe9 the run.\n",
			want: plandoc.ErrNotUTF8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := plandoc.Parse([]byte(tt.src))
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("Parse = error %v, want %v", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse = error %v, want a document", err)
			}
			if doc.Len() != tt.steps {
				t.Errorf("Len() = %d, want %d", doc.Len(), tt.steps)
			}
			if got := string(doc.Bytes()); got != tt.src {
				t.Errorf("Bytes() != the plan as it was left\n got: %q\nwant: %q", got, tt.src)
			}
			if approve.Digest(doc.Bytes()) != approve.Digest([]byte(tt.src)) {
				t.Errorf("approve.Digest moved for a plan nobody edited")
			}
		})
	}
}
