package belaytest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

func TestFakeReviewerZeroValue(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeReviewer{}

	report, err := f.Review(context.Background(), belay.ReviewRequest{WorkDir: "/repo"})
	if err != nil {
		t.Errorf("Review() returned error: %v", err)
	}
	if !reflect.DeepEqual(report, belay.QualityReport{}) {
		t.Errorf("Review() = %+v, want zero value", report)
	}
	if got := f.CallCount(); got != 1 {
		t.Errorf("CallCount() = %d, want 1", got)
	}
}

func TestFakeReviewerScriptedResponses(t *testing.T) {
	t.Parallel()

	errUnreachable := errors.New("sonar mcp server unreachable")
	f := &belaytest.FakeReviewer{
		Responses: []belay.QualityReport{
			{Source: "sonar-mcp-ai-review", Gate: belay.GatePass, Issues: []belay.Issue{}},
		},
		Errs: []error{errUnreachable, nil},
	}
	ctx := context.Background()
	req := belay.ReviewRequest{WorkDir: "/repo", FailOn: belay.SeverityMajor}

	if _, err := f.Review(ctx, req); !errors.Is(err, errUnreachable) {
		t.Errorf("call 1 err = %v, want %v", err, errUnreachable)
	}
	report, err := f.Review(ctx, req)
	if err != nil {
		t.Fatalf("call 2 returned error: %v", err)
	}
	if report.Gate != belay.GatePass {
		t.Errorf("call 2 Gate = %v, want GatePass", report.Gate)
	}
}

func TestFakeReviewerFuncSeesFailOn(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeReviewer{
		Func: func(_ context.Context, req belay.ReviewRequest) (belay.QualityReport, error) {
			gate := belay.GatePass
			if req.FailOn == belay.SeverityInfo {
				gate = belay.GateFail
			}
			return belay.QualityReport{Gate: gate, Issues: []belay.Issue{}}, nil
		},
	}
	ctx := context.Background()

	strict, err := f.Review(ctx, belay.ReviewRequest{FailOn: belay.SeverityInfo})
	if err != nil {
		t.Fatal(err)
	}
	if strict.Gate != belay.GateFail {
		t.Errorf("strict FailOn: Gate = %v, want GateFail", strict.Gate)
	}

	lenient, err := f.Review(ctx, belay.ReviewRequest{FailOn: belay.SeverityBlocker})
	if err != nil {
		t.Fatal(err)
	}
	if lenient.Gate != belay.GatePass {
		t.Errorf("lenient FailOn: Gate = %v, want GatePass", lenient.Gate)
	}

	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() has %d entries, want 2", len(calls))
	}
}

var _ belay.Reviewer = (*belaytest.FakeReviewer)(nil)
