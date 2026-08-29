package belay_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/belay-dev/belay/pkg/belay"
)

func TestTestReportOK(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		report belay.TestReport
		want   bool
	}{
		"zero value is not OK (no tests ran)": {
			report: belay.TestReport{},
			want:   false,
		},
		"all passed": {
			report: belay.TestReport{Total: 10, Passed: 10},
			want:   true,
		},
		"some failed": {
			report: belay.TestReport{Total: 10, Passed: 8, Failed: 2},
			want:   false,
		},
		"build failure synthesized as one failure": {
			report: belay.TestReport{Total: 0, Failed: 1, Failures: []belay.TestFailure{{Name: "build"}}},
			want:   false,
		},
		"passed with skips (total > passed, zero failed)": {
			report: belay.TestReport{Total: 10, Passed: 7, Failed: 0},
			want:   true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := tt.report.OK(); got != tt.want {
				t.Errorf("TestReport%+v.OK() = %v, want %v", tt.report, got, tt.want)
			}
		})
	}
}

func TestTestReportJSONRoundTrip(t *testing.T) {
	t.Parallel()

	want := belay.TestReport{
		Total:  12,
		Passed: 10,
		Failed: 2,
		Failures: []belay.TestFailure{
			{Name: "TestFoo", File: "foo_test.go", Line: 10, Message: "assertion failed"},
			{Name: "TestBar/sub", File: "bar_test.go", Line: 22, Message: "panic: nil pointer"},
		},
		Duration: 3500 * time.Millisecond,
		Raw:      json.RawMessage(`{"events":[]}`),
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var got belay.TestReport
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	if diff := cmp.Diff(want, got, rawMessageComparer); diff != "" {
		t.Errorf("round trip through %s changed the value (-want +got):\n%s", encoded, diff)
	}
}

// TestTestReportDurationIsNanoseconds pins the (stdlib-default, unsurprising
// once you know it) wire representation of Duration: a plain integer count
// of nanoseconds, not a formatted string.
func TestTestReportDurationIsNanoseconds(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(belay.TestReport{Duration: 2 * time.Second})
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		t.Fatalf("json.Unmarshal(%s) returned error: %v", encoded, err)
	}
	if got, want := string(asMap["duration"]), "2000000000"; got != want {
		t.Errorf(`TestReport{Duration: 2s} serialized "duration" as %s, want %s`, got, want)
	}
}
