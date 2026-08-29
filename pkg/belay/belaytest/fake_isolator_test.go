package belaytest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/belay-dev/belay/pkg/belay"
	"github.com/belay-dev/belay/pkg/belay/belaytest"
)

func TestFakeIsolatorZeroValueIsNoIsolation(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeIsolator{}

	ws, err := f.Create(context.Background(), "/src/repo", "run-1")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	want := belay.Workspace{ID: "run-1", Dir: "/src/repo"}
	if ws != want {
		t.Errorf("Create() = %+v, want %+v", ws, want)
	}

	if err := f.Destroy(context.Background(), ws); err != nil {
		t.Errorf("Destroy returned error: %v", err)
	}
}

func TestFakeIsolatorScriptedCreate(t *testing.T) {
	t.Parallel()

	errNoSpace := errors.New("no space left on device")
	f := &belaytest.FakeIsolator{
		CreateResponses: []belay.Workspace{
			{ID: "a", Dir: "/tmp/a", Ephemeral: true},
		},
		CreateErrs: []error{nil, errNoSpace},
	}
	ctx := context.Background()

	ws1, err := f.Create(ctx, "/src", "a")
	if err != nil {
		t.Fatalf("call 1 returned error: %v", err)
	}
	if ws1.ID != "a" || !ws1.Ephemeral {
		t.Errorf("call 1 = %+v, want ID=a Ephemeral=true", ws1)
	}

	if _, err := f.Create(ctx, "/src", "b"); !errors.Is(err, errNoSpace) {
		t.Errorf("call 2 err = %v, want %v", err, errNoSpace)
	}
}

func TestFakeIsolatorLeaked(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	f := &belaytest.FakeIsolator{
		CreateErrs: []error{nil, nil, errBoom},
	}
	ctx := context.Background()

	wsA, err := f.Create(ctx, "/src", "a")
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := f.Create(ctx, "/src", "b")
	if err != nil {
		t.Fatal(err)
	}
	// A failed Create must not appear in Leaked(): there is nothing to
	// destroy, per Isolator.Create's contract.
	if _, err := f.Create(ctx, "/src", "c"); err == nil {
		t.Fatal("expected call 3 to error")
	}

	if leaked := f.Leaked(); len(leaked) != 2 {
		t.Fatalf("Leaked() before any Destroy = %v, want both a and b", leaked)
	}

	if err := f.Destroy(ctx, wsA); err != nil {
		t.Fatal(err)
	}

	leaked := f.Leaked()
	if len(leaked) != 1 || leaked[0].ID != "b" {
		t.Fatalf("Leaked() after destroying a = %v, want only b", leaked)
	}

	if err := f.Destroy(ctx, wsB); err != nil {
		t.Fatal(err)
	}
	if leaked := f.Leaked(); len(leaked) != 0 {
		t.Fatalf("Leaked() after destroying everything = %v, want none", leaked)
	}
}

func TestFakeIsolatorCreateFuncAndDestroyFunc(t *testing.T) {
	t.Parallel()

	var destroyedIDs []string
	f := &belaytest.FakeIsolator{
		CreateFunc: func(_ context.Context, src, id string) (belay.Workspace, error) {
			return belay.Workspace{ID: id, Dir: src + "/" + id, Ephemeral: true}, nil
		},
		DestroyFunc: func(_ context.Context, ws belay.Workspace) error {
			destroyedIDs = append(destroyedIDs, ws.ID)
			return nil
		},
	}
	ctx := context.Background()

	ws, err := f.Create(ctx, "/src", "candidate-0")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/src/candidate-0"; ws.Dir != want {
		t.Errorf("Dir = %q, want %q", ws.Dir, want)
	}

	if err := f.Destroy(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if len(destroyedIDs) != 1 || destroyedIDs[0] != "candidate-0" {
		t.Errorf("DestroyFunc was called with %v, want [candidate-0]", destroyedIDs)
	}
	if leaked := f.Leaked(); len(leaked) != 0 {
		t.Errorf("Leaked() = %v, want none", leaked)
	}
}

func TestFakeIsolatorRecordsCalls(t *testing.T) {
	t.Parallel()

	f := &belaytest.FakeIsolator{}
	ctx := context.Background()

	ws, err := f.Create(ctx, "/src", "id-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Destroy(ctx, ws); err != nil {
		t.Fatal(err)
	}

	creates := f.CreateCalls()
	if len(creates) != 1 || creates[0] != (belaytest.CreateCall{Src: "/src", ID: "id-1"}) {
		t.Errorf("CreateCalls() = %+v, want one call{Src: /src, ID: id-1}", creates)
	}

	destroys := f.DestroyCalls()
	if len(destroys) != 1 || destroys[0].ID != "id-1" {
		t.Errorf("DestroyCalls() = %+v, want one workspace with ID id-1", destroys)
	}
}

var _ belay.Isolator = (*belaytest.FakeIsolator)(nil)
