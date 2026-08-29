package detect

import (
	"encoding/json"
	"errors"
	"testing"
)

// allKinds is every declared Kind, including the configuration-only values.
var allKinds = []Kind{KindUnknown, KindAuto, KindGo, KindNode, KindPython, KindCustom}

func TestKindRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allKinds {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			got, err := ParseKind(want.String())
			if err != nil {
				t.Fatalf("ParseKind(%q) returned error: %v", want.String(), err)
			}
			if got != want {
				t.Errorf("ParseKind(%q) = %v, want %v", want.String(), got, want)
			}
		})
	}
}

func TestKindStringIsCanonicalConfigValue(t *testing.T) {
	t.Parallel()

	// These are the exact spellings the test.runner config key accepts.
	tests := map[Kind]string{
		KindUnknown: "unknown",
		KindAuto:    "auto",
		KindGo:      "go",
		KindNode:    "node",
		KindPython:  "python",
		KindCustom:  "custom",
	}

	for kind, want := range tests {
		if got := kind.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}

func TestParseKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Kind
		wantErr bool
	}{
		{name: "exact", input: "go", want: KindGo},
		{name: "uppercase", input: "GO", want: KindGo},
		{name: "mixed case", input: "PyThOn", want: KindPython},
		{name: "surrounding whitespace", input: "  node\t", want: KindNode},
		{name: "config only auto", input: "auto", want: KindAuto},
		{name: "config only custom", input: "custom", want: KindCustom},
		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "unknown word", input: "rust", wantErr: true},
		{name: "near miss", input: "golang", wantErr: true},
		{name: "numeric", input: "2", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseKind(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseKind(%q) = %v, want error", tt.input, got)
				}
				if !errors.Is(err, ErrUnknownKind) {
					t.Errorf("ParseKind(%q) error = %v, want it to wrap ErrUnknownKind", tt.input, err)
				}
				if got != KindUnknown {
					t.Errorf("ParseKind(%q) = %v on error, want KindUnknown", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKind(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ParseKind(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestKindOutOfRangeDoesNotPanic(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{-1, 99} {
		if got := kind.String(); got == "" {
			t.Errorf("Kind(%d).String() = empty, want a placeholder", int(kind))
		}
		if _, err := kind.MarshalText(); err == nil {
			t.Errorf("Kind(%d).MarshalText() = nil error, want error", int(kind))
		}
	}
}

func TestKindJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, want := range allKinds {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("json.Marshal(%v) returned error: %v", want, err)
		}

		var got Kind
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
		}
		if got != want {
			t.Errorf("round trip of %v through %s = %v", want, encoded, got)
		}
	}
}

func TestKindUnmarshalTextRejectsGarbage(t *testing.T) {
	t.Parallel()

	var k Kind
	if err := k.UnmarshalText([]byte("haskell")); err == nil {
		t.Fatal("UnmarshalText(\"haskell\") = nil error, want error")
	} else if !errors.Is(err, ErrUnknownKind) {
		t.Errorf("UnmarshalText error = %v, want it to wrap ErrUnknownKind", err)
	}
}

func TestEnumStringsAreStable(t *testing.T) {
	t.Parallel()

	// These strings are part of the package's contract: they appear in config
	// files, logs and golden output. Changing one is a breaking change.
	confidences := map[Confidence]string{
		ConfidenceNone: "none", ConfidenceLow: "low",
		ConfidenceMedium: "medium", ConfidenceHigh: "high",
	}
	for value, want := range confidences {
		if got := value.String(); got != want {
			t.Errorf("Confidence(%d).String() = %q, want %q", int(value), got, want)
		}
	}

	managers := map[PackageManager]string{
		PackageManagerUnknown: "unknown", PackageManagerNPM: "npm",
		PackageManagerYarn: "yarn", PackageManagerPNPM: "pnpm",
		PackageManagerBun: "bun",
	}
	for value, want := range managers {
		if got := value.String(); got != want {
			t.Errorf("PackageManager(%d).String() = %q, want %q", int(value), got, want)
		}
	}

	frameworks := map[TestFramework]string{
		TestFrameworkUnknown: "unknown", TestFrameworkJest: "jest",
		TestFrameworkVitest: "vitest", TestFrameworkMocha: "mocha",
		TestFrameworkNodeTest: "node:test",
	}
	for value, want := range frameworks {
		if got := value.String(); got != want {
			t.Errorf("TestFramework(%d).String() = %q, want %q", int(value), got, want)
		}
	}
}

func TestConfidenceOrdering(t *testing.T) {
	t.Parallel()

	// Detect sorts by descending Confidence, so the ordinals must increase
	// with strength.
	ordered := []Confidence{ConfidenceNone, ConfidenceLow, ConfidenceMedium, ConfidenceHigh}
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1] >= ordered[i] {
			t.Fatalf("Confidence %v is not less than %v", ordered[i-1], ordered[i])
		}
	}
}

func TestEnumStringsOutOfRangeDoNotPanic(t *testing.T) {
	t.Parallel()

	// Every enum in this package renders an out-of-range value as a
	// placeholder, so a value from a stale config or a corrupted journal can
	// be logged rather than crashing the dispatcher.
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "confidence below range", got: Confidence(-1).String(), want: "Confidence(-1)"},
		{name: "confidence above range", got: Confidence(99).String(), want: "Confidence(99)"},
		{name: "package manager below range", got: PackageManager(-1).String(), want: "PackageManager(-1)"},
		{name: "package manager above range", got: PackageManager(99).String(), want: "PackageManager(99)"},
		{name: "test framework below range", got: TestFramework(-1).String(), want: "TestFramework(-1)"},
		{name: "test framework above range", got: TestFramework(99).String(), want: "TestFramework(99)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.got != tt.want {
				t.Errorf("String() = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestProblemString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		problem Problem
		want    string
	}{
		{
			name:    "with a cause",
			problem: Problem{File: "go.mod", Err: ErrMalformedMarker},
			want:    "go.mod: malformed marker file",
		},
		{
			name:    "without a cause",
			problem: Problem{File: "go.mod"},
			want:    "go.mod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.problem.String(); got != tt.want {
				t.Errorf("Problem.String() = %q, want %q", got, tt.want)
			}
		})
	}
}
