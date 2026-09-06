package belaytest_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dhaam-ai/belay/pkg/belay"
	"github.com/dhaam-ai/belay/pkg/belay/belaytest"
)

func TestFakeRunnerZeroValue(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeRunner{}

	if got := f.Name(); got != "fake-runner" {
		t.Errorf("Name() = %q, want %q", got, "fake-runner")
	}
	if got := f.Detect("/some/dir"); got != false {
		t.Errorf("Detect() = %v, want false", got)
	}

	report, err := f.Test(context.Background(), "/some/dir")
	if err != nil {
		t.Errorf("Test() returned error: %v", err)
	}
	if !reflect.DeepEqual(report, belay.TestReport{}) {
		t.Errorf("Test() = %+v, want zero value", report)
	}
}

func TestFakeRunnerDetectValueAndFunc(t *testing.T) {
	t.Parallel()

	staticFake := &belaytest.FakeRunner{DetectValue: true}
	if !staticFake.Detect("anything") {
		t.Error("Detect() with DetectValue: true = false, want true")
	}

	dynamicFake := &belaytest.FakeRunner{
		DetectFunc: func(dir string) bool { return dir == "/go-project" },
	}
	if !dynamicFake.Detect("/go-project") {
		t.Error("Detect(/go-project) = false, want true")
	}
	if dynamicFake.Detect("/other") {
		t.Error("Detect(/other) = true, want false")
	}
}

func TestFakeRunnerScriptedTestResponses(t *testing.T) {
	t.Parallel()

	errToolchain := errors.New("go: not found")
	f := &belaytest.FakeRunner{
		Responses: []belay.TestReport{
			{},
			{Total: 5, Passed: 5},
		},
		Errs: []error{errToolchain, nil},
	}
	ctx := context.Background()

	if _, err := f.Test(ctx, "/dir"); !errors.Is(err, errToolchain) {
		t.Errorf("call 1 err = %v, want %v", err, errToolchain)
	}
	report, err := f.Test(ctx, "/dir")
	if err != nil {
		t.Fatalf("call 2 returned error: %v", err)
	}
	if !report.OK() {
		t.Errorf("call 2 report %+v, want OK", report)
	}
	// Sticky-last: a third call repeats call 2's outcome.
	report, err = f.Test(ctx, "/dir")
	if err != nil || !report.OK() {
		t.Errorf("call 3 = (%+v, %v), want sticky repeat of call 2", report, err)
	}
}

func TestFakeRunnerRecordsCalls(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeRunner{DetectValue: true}
	f.Detect("/a")
	f.Detect("/b")
	if _, err := f.Test(context.Background(), "/a"); err != nil {
		t.Fatal(err)
	}

	if got := f.DetectCalls(); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Errorf("DetectCalls() = %v, want [/a /b]", got)
	}
	if got := f.TestCalls(); len(got) != 1 || got[0] != "/a" {
		t.Errorf("TestCalls() = %v, want [/a]", got)
	}
	if got := f.CallCount(); got != 1 {
		t.Errorf("CallCount() = %d, want 1", got)
	}
}

var _ belay.TestRunner = (*belaytest.FakeRunner)(nil)
