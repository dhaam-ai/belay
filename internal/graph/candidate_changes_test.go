package graph_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/dhaam-ai/belay/internal/filerecord"
	"github.com/dhaam-ai/belay/internal/graph"
	"github.com/dhaam-ai/belay/internal/state"
	"github.com/dhaam-ai/belay/pkg/belay"
)

// seedingIsolator makes a candidate's workspace the way a directory copy
// does: a directory already holding the files the candidate starts from.
type seedingIsolator struct{ *fakeIsolator }

func (s seedingIsolator) Create(ctx context.Context, src, id string) (belay.Workspace, error) {
	ws, err := s.fakeIsolator.Create(ctx, src, id)
	if err != nil || ws.Dir == "" {
		return ws, err
	}
	return ws, os.WriteFile(filepath.Join(ws.Dir, "seed.txt"), []byte("what the copy held\n"), 0o600)
}

// recordSources is the ChangeSource factory a run with a file record gets:
// the record in the run's own directory.
func recordSources(man *state.Manifest, layout state.Layout) graph.ChangeSource {
	if man.Changes == nil || man.Changes.From != state.ChangesFromRecord {
		return nil
	}
	return filerecord.Source{Path: layout.FileRecordPath()}
}

// A fanout candidate made by a directory copy has no git to ask what it
// changed, so its ground truth is a record of its workspace taken when the
// workspace was made, before its agent runs. The candidate inherits where its
// change is read from with the rest of its parent's pins, and its nodes are
// handed a source that answers from its own record: what the agent created
// and what it altered, whatever it reported.
func TestACandidateWithNoGitIsJudgedAgainstARecordOfItsWorkspace(t *testing.T) {
	h := newRun(t, budgetCfg(3.00, 0), "ship it")
	pin := state.Changes{From: state.ChangesFromRecord}
	man, err := h.store.LoadManifest()
	if err != nil {
		t.Fatalf("LoadManifest() error = %v", err)
	}
	man.SetChanges(pin)
	if err := h.store.SaveManifest(man); err != nil {
		t.Fatalf("SaveManifest() error = %v", err)
	}

	var (
		mu   sync.Mutex
		seen = map[string][]string{}
	)
	code := newCandidateNode(graph.NodeCode, func(ctx context.Context, id string, rc *graph.RunContext) (graph.Result, error) {
		if rc.Changes == nil {
			t.Errorf("%s was given no ChangeSource", id)
			return finishes(0.10)(ctx, id, rc)
		}
		// The agent's work: one new file, one altered.
		for name, body := range map[string]string{"agent.txt": "new\n", "seed.txt": "altered\n"} {
			if err := os.WriteFile(filepath.Join(rc.Workspace, name), []byte(body), 0o600); err != nil {
				t.Errorf("write %s: %v", name, err)
			}
		}
		got, err := rc.Changes.Changes(ctx, rc.Workspace)
		if err != nil {
			t.Errorf("%s Changes() error = %v", id, err)
		}
		mu.Lock()
		seen[id] = got
		mu.Unlock()
		return finishes(0.10)(ctx, id, rc)
	})
	node := &runsCandidates{specs: poolSpecs(h, 2)}
	iso := seedingIsolator{newFakeIsolator(h.layout.CandidatesDir())}

	if _, err := h.poolDispatcher(t, pool{registry: candidateRegistry(t, code), isolator: iso, changes: recordSources}, node).
		Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if node.err != nil {
		t.Fatalf("CandidateRuns.Run() error = %v, want nil", node.err)
	}
	for _, id := range []string{"cand-01", "cand-02"} {
		if want := []string{"agent.txt", "seed.txt"}; !reflect.DeepEqual(seen[id], want) {
			t.Errorf("%s changed %q by its own record, want %q", id, seen[id], want)
		}
		// The parent's pin, and the record the candidate's own run now holds.
		want := pin
		want.Record = true
		if cm := candidateManifest(t, h, id); cm.Changes == nil || *cm.Changes != want {
			t.Errorf("%s manifest Changes = %+v, want %+v", id, cm.Changes, want)
		}
	}
}
