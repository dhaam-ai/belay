package belaytest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

func TestFakeLinterZeroValue(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeLinter{}

	if got := f.Name(); got != "fake-linter" {
		t.Errorf("Name() = %q, want %q", got, "fake-linter")
	}
	if got := f.Detect("/dir"); got != false {
		t.Errorf("Detect() = %v, want false", got)
	}

	report, err := f.Lint(context.Background(), "/dir")
	if err != nil {
		t.Errorf("Lint() returned error: %v", err)
	}
	if !reflect.DeepEqual(report, belay.QualityReport{}) {
		t.Errorf("Lint() = %+v, want zero value", report)
	}
}

func TestFakeLinterScriptedResponses(t *testing.T) {
	t.Parallel()

	errMissing := errors.New("golangci-lint: not found")
	f := &belaytest.FakeLinter{
		DetectValue: true,
		Responses: []belay.QualityReport{
			{Source: "fake-linter", Gate: belay.GatePass, Issues: []belay.Issue{}},
		},
		Errs: []error{errMissing, nil},
	}
	ctx := context.Background()

	if !f.Detect("/dir") {
		t.Fatal("Detect() = false, want true")
	}
	if _, err := f.Lint(ctx, "/dir"); !errors.Is(err, errMissing) {
		t.Errorf("call 1 err = %v, want %v", err, errMissing)
	}
	report, err := f.Lint(ctx, "/dir")
	if err != nil {
		t.Fatalf("call 2 returned error: %v", err)
	}
	if report.Gate != belay.GatePass {
		t.Errorf("call 2 Gate = %v, want GatePass (sticky-last)", report.Gate)
	}
}

func TestFakeLinterFunc(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeLinter{
		Func: func(_ context.Context, dir string) (belay.QualityReport, error) {
			return belay.QualityReport{Source: "fake-linter", Summary: "checked " + dir}, nil
		},
	}

	report, err := f.Lint(context.Background(), "/repo")
	if err != nil {
		t.Fatalf("Lint returned error: %v", err)
	}
	if want := "checked /repo"; report.Summary != want {
		t.Errorf("Summary = %q, want %q", report.Summary, want)
	}
	if got := f.LintCalls(); len(got) != 1 || got[0] != "/repo" {
		t.Errorf("LintCalls() = %v, want [/repo]", got)
	}
	if got := f.CallCount(); got != 1 {
		t.Errorf("CallCount() = %d, want 1", got)
	}
}

var _ belay.Linter = (*belaytest.FakeLinter)(nil)
