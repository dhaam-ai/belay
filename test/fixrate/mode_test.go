//go:build fixrate

package fixrate

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"replay", ModeReplay, false},
		{"REPLAY", ModeReplay, false},
		{"  replay  ", ModeReplay, false},
		{"live", ModeLive, false},
		{"LIVE", ModeLive, false},
		{"", "", true},
		{"fake", "", true},
		{"replayy", "", true},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %v, nil; want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) unexpected error: %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("ParseMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMode_Valid(t *testing.T) {
	t.Parallel()
	if !ModeReplay.Valid() {
		t.Error("ModeReplay.Valid() = false, want true")
	}
	if !ModeLive.Valid() {
		t.Error("ModeLive.Valid() = false, want true")
	}
	if Mode("bogus").Valid() {
		t.Error(`Mode("bogus").Valid() = true, want false`)
	}
	if Mode("").Valid() {
		t.Error(`Mode("").Valid() = true, want false`)
	}
}

// The provenance text is a contract, not prose: it must say, unambiguously
// and in words, which of "measures the graph" or "measures a real agent"
// a Result came from. See requirement 2's "the difference must be
// impossible to confuse."
func TestMode_Provenance_IsUnambiguous(t *testing.T) {
	t.Parallel()

	replay := ModeReplay.provenance()
	live := ModeLive.provenance()

	for _, want := range []string{"replay", "NO real coding agent", "not a measure"} {
		if !strings.Contains(strings.ToLower(replay), strings.ToLower(want)) {
			t.Errorf("ModeReplay.provenance() = %q, want it to mention %q", replay, want)
		}
	}
	if !strings.Contains(live, "live") || !strings.Contains(live, "real spend") {
		t.Errorf("ModeLive.provenance() = %q, want it to mention live spend", live)
	}
	if strings.Contains(replay, "real spend") {
		t.Errorf("ModeReplay.provenance() = %q, must not claim real spend", replay)
	}
	if strings.Contains(live, "zero-cost") {
		t.Errorf("ModeLive.provenance() = %q, must not claim zero cost", live)
	}
}
