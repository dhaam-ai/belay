package vcs_test

import (
	"testing"

	"github.com/dhaam-ai/belay/internal/vcs"
)

// TestMessageText pins the one layout every Repository writes: the subject,
// then the body and the trailers as paragraphs of their own, one blank line
// apart, with nothing after the last line. git reads trailers only from the
// last paragraph, so a blank line out of place would turn a run's ID into
// prose.
func TestMessageText(t *testing.T) {
	runID := vcs.Trailer{Key: "Belay-Run", Value: "20260919T101500Z-1f2e3d4c5b6a"}
	candidate := vcs.Trailer{Key: "Belay-Candidate", Value: "cand-02-a1"}

	tests := []struct {
		name string
		msg  vcs.Message
		want string
	}{
		{
			name: "subject only",
			msg:  vcs.Message{Subject: "belay: add a --json flag"},
			want: "belay: add a --json flag",
		},
		{
			name: "subject and body",
			msg:  vcs.Message{Subject: "belay: add a --json flag", Body: "The CLI prints JSON.\nTests cover it."},
			want: "belay: add a --json flag\n\nThe CLI prints JSON.\nTests cover it.",
		},
		{
			name: "subject, body and trailers",
			msg: vcs.Message{
				Subject:  "belay: add a --json flag",
				Body:     "The CLI prints JSON.",
				Trailers: []vcs.Trailer{runID, candidate},
			},
			want: "belay: add a --json flag\n\nThe CLI prints JSON.\n\n" +
				"Belay-Run: 20260919T101500Z-1f2e3d4c5b6a\nBelay-Candidate: cand-02-a1",
		},
		{
			name: "an empty body omits its paragraph",
			msg:  vcs.Message{Subject: "belay: add a --json flag", Trailers: []vcs.Trailer{runID}},
			want: "belay: add a --json flag\n\nBelay-Run: 20260919T101500Z-1f2e3d4c5b6a",
		},
		{
			name: "empty trailers omit their paragraph",
			msg:  vcs.Message{Subject: "belay: add a --json flag", Body: "The CLI prints JSON.", Trailers: []vcs.Trailer{}},
			want: "belay: add a --json flag\n\nThe CLI prints JSON.",
		},
		{
			name: "trailers keep the order they were given in",
			msg:  vcs.Message{Subject: "belay: s", Trailers: []vcs.Trailer{{Key: "Z-Last", Value: "1"}, {Key: "A-First", Value: "2"}}},
			want: "belay: s\n\nZ-Last: 1\nA-First: 2",
		},
		{
			name: "a body ending in blank lines leaves no trailing blank line",
			msg:  vcs.Message{Subject: "belay: s", Body: "The CLI prints JSON.\n\n\n"},
			want: "belay: s\n\nThe CLI prints JSON.",
		},
		{
			name: "a body that is only whitespace is empty",
			msg:  vcs.Message{Subject: "belay: s", Body: " \n\t\n", Trailers: []vcs.Trailer{runID}},
			want: "belay: s\n\nBelay-Run: 20260919T101500Z-1f2e3d4c5b6a",
		},
		{
			name: "whitespace around subject and body leaves one blank line between paragraphs",
			msg:  vcs.Message{Subject: "belay: s\n", Body: "\n\nThe CLI prints JSON.\n", Trailers: []vcs.Trailer{runID}},
			want: "belay: s\n\nThe CLI prints JSON.\n\nBelay-Run: 20260919T101500Z-1f2e3d4c5b6a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.msg.Text(); got != tt.want {
				t.Errorf("Text() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}
