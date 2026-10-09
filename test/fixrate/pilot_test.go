//go:build fixrate

package fixrate

import (
	"cmp"
	"fmt"
	"slices"
	"testing"
)

// pilotCorpus returns synthetic tasks over five repositories of 10, 5, 3, 1
// and 1 tasks.
func pilotCorpus() []Task {
	var tasks []Task
	for repo, n := range map[string]int{"example/alpha": 10, "example/bravo": 5, "example/charlie": 3, "example/delta": 1, "example/echo": 1} {
		for i := range n {
			tasks = append(tasks, Task{ID: fmt.Sprintf("%s-%02d", repo[len("example/"):], i), Repo: repo})
		}
	}
	slices.SortFunc(tasks, func(a, b Task) int { return cmp.Compare(a.ID, b.ID) })
	return tasks
}

// perRepo counts tasks by repository.
func perRepo(tasks []Task) map[string]int {
	out := map[string]int{}
	for _, t := range tasks {
		out[t.Repo]++
	}
	return out
}

// TestStratifiedPilot pins the allocation. Every repository gets a task when
// the pilot is big enough for that, because a pilot exists to shake out what
// goes wrong per repository -- a test command that does not run, an image
// that does not start -- before a full run pays for it on every task. The
// remaining tasks are shared in proportion to what is left in each
// repository, by largest remainder, so a pilot looks like the corpus.
func TestStratifiedPilot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want map[string]int
	}{
		{
			// 5 seats give each repository one; 3 more go by largest
			// remainder over what is left (9, 4, 2, 0, 0): quotas 1.8,
			// 0.8 and 0.4, so alpha gets 1, and the last 2 go to the
			// largest fractions, alpha's and bravo's 0.8.
			name: "every repository, then in proportion",
			n:    8,
			want: map[string]int{"example/alpha": 3, "example/bravo": 2, "example/charlie": 1, "example/delta": 1, "example/echo": 1},
		},
		{
			// Too few seats for one each: largest remainder over the
			// repositories' sizes (quotas 1.0, 0.5, 0.3, 0.1, 0.1).
			name: "fewer tasks than repositories",
			n:    2,
			want: map[string]int{"example/alpha": 1, "example/bravo": 1},
		},
		{
			name: "the whole corpus",
			n:    20,
			want: map[string]int{"example/alpha": 10, "example/bravo": 5, "example/charlie": 3, "example/delta": 1, "example/echo": 1},
		},
		{
			name: "more than the whole corpus is the whole corpus",
			n:    50,
			want: map[string]int{"example/alpha": 10, "example/bravo": 5, "example/charlie": 3, "example/delta": 1, "example/echo": 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			corpus := pilotCorpus()
			got, err := StratifiedPilot(corpus, tt.n, 7)
			if err != nil {
				t.Fatalf("StratifiedPilot() error = %v", err)
			}
			if counts := perRepo(got); !mapsEqual(counts, tt.want) {
				t.Errorf("per repository = %v, want %v", counts, tt.want)
			}
			if !slices.IsSortedFunc(got, func(a, b Task) int { return cmp.Compare(a.ID, b.ID) }) {
				t.Errorf("the pilot is not in task order: %v", ids(got))
			}
			for _, task := range got {
				if !slices.ContainsFunc(corpus, func(c Task) bool { return c.ID == task.ID }) {
					t.Errorf("the pilot holds %q, which is not in the corpus", task.ID)
				}
			}
		})
	}
}

// TestStratifiedPilot_IsReproducible: the same corpus, size and seed always
// choose the same pilot, so a pilot named in a report can be chosen again;
// another seed chooses differently within a repository.
func TestStratifiedPilot_IsReproducible(t *testing.T) {
	t.Parallel()

	first, err := StratifiedPilot(pilotCorpus(), 8, 7)
	if err != nil {
		t.Fatalf("StratifiedPilot() error = %v", err)
	}
	again, err := StratifiedPilot(pilotCorpus(), 8, 7)
	if err != nil {
		t.Fatalf("StratifiedPilot() error = %v", err)
	}
	if !slices.Equal(ids(first), ids(again)) {
		t.Errorf("the same seed chose %v and then %v", ids(first), ids(again))
	}
	differs := false
	for seed := uint64(8); seed < 40 && !differs; seed++ {
		other, err := StratifiedPilot(pilotCorpus(), 8, seed)
		if err != nil {
			t.Fatalf("StratifiedPilot() error = %v", err)
		}
		differs = !slices.Equal(ids(first), ids(other))
	}
	if !differs {
		t.Error("no other seed chose a different pilot; the seed is not reaching the choice")
	}
}

func TestStratifiedPilot_Refusals(t *testing.T) {
	t.Parallel()

	if _, err := StratifiedPilot(pilotCorpus(), 0, 7); err == nil {
		t.Error("StratifiedPilot(n=0) = nil error, want a refusal")
	}
	dup := append(pilotCorpus(), pilotCorpus()[0])
	if _, err := StratifiedPilot(dup, 5, 7); err == nil {
		t.Error("StratifiedPilot() with a task twice = nil error, want a refusal")
	}
}

func ids(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
