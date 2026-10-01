//go:build fixrate

package fixrate

import (
	"cmp"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
)

// errPilot reports a pilot StratifiedPilot cannot choose.
var errPilot = errors.New("fixrate: unusable pilot")

// StratifiedPilot chooses n of tasks for a pilot, stratified by repository,
// and returns them in task id order.
//
// # Why a pilot, and why stratified
//
// A full corpus is the expensive run. The pilot is the cheap one before it,
// and exists to find what goes wrong per repository -- a test command that
// does not run in a repository's image, a container that will not start, a
// harness that cannot grade -- while it costs a few tasks to find, not the
// whole repository's share of a full run. So every repository gets a task
// whenever n allows one each, and the remaining tasks are shared in
// proportion to what is left in each repository, so the pilot's resolve
// rates look like the corpus's rather than like its largest repository's.
//
// # How
//
// Seats are shared by the largest-remainder method over integer arithmetic,
// so no rounding decides a tie; a tie between equal remainders goes to the
// larger repository, then to the earlier name. With fewer seats than
// repositories, the whole of n is shared that way over the repositories'
// sizes, and the smallest go without. Within a repository the choice is a
// shuffle seeded by seed and the repository's name, so the same corpus, n and
// seed always choose the same pilot, and a pilot named in a report -- "50
// tasks, seed 1" -- can be chosen again by anyone holding the records.
//
// A pilot as large as the corpus, or larger, is the corpus.
func StratifiedPilot(tasks []Task, n int, seed uint64) ([]Task, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: a pilot of %d task(s)", errPilot, n)
	}
	byRepo := map[string][]Task{}
	seen := map[string]bool{}
	for _, t := range tasks {
		if seen[t.ID] {
			return nil, fmt.Errorf("%w: task %q appears twice", errPilot, t.ID)
		}
		seen[t.ID] = true
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}
	if n >= len(tasks) {
		return sortedTasks(slices.Clone(tasks)), nil
	}

	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	slices.Sort(repos)

	seats := make(map[string]int, len(repos))
	weights := make(map[string]int, len(repos))
	remaining := n
	if n >= len(repos) {
		for _, repo := range repos {
			seats[repo] = 1
			weights[repo] = len(byRepo[repo]) - 1
		}
		remaining = n - len(repos)
	} else {
		for _, repo := range repos {
			weights[repo] = len(byRepo[repo])
		}
	}
	for repo, extra := range largestRemainder(repos, weights, remaining) {
		seats[repo] += extra
	}

	var pilot []Task
	for _, repo := range repos {
		stratum := sortedTasks(slices.Clone(byRepo[repo]))
		h := fnv.New64a()
		if _, err := h.Write([]byte(repo)); err != nil {
			return nil, fmt.Errorf("fixrate: hashing %q: %w", repo, err)
		}
		rng := rand.New(rand.NewPCG(seed, h.Sum64())) //nolint:gosec // a reproducible sample, not a secret
		rng.Shuffle(len(stratum), func(i, j int) { stratum[i], stratum[j] = stratum[j], stratum[i] })
		pilot = append(pilot, stratum[:seats[repo]]...)
	}
	return sortedTasks(pilot), nil
}

// largestRemainder shares seats among keys in proportion to weights by the
// largest-remainder method, in integers; see StratifiedPilot.
func largestRemainder(keys []string, weights map[string]int, seats int) map[string]int {
	out := make(map[string]int, len(keys))
	total := 0
	for _, k := range keys {
		total += weights[k]
	}
	if seats <= 0 || total == 0 {
		return out
	}
	type rem struct {
		key    string
		weight int
		rest   int
	}
	rests := make([]rem, 0, len(keys))
	given := 0
	for _, k := range keys {
		share := seats * weights[k]
		out[k] = share / total
		given += out[k]
		rests = append(rests, rem{key: k, weight: weights[k], rest: share % total})
	}
	slices.SortStableFunc(rests, func(a, b rem) int {
		if c := cmp.Compare(b.rest, a.rest); c != 0 {
			return c
		}
		if c := cmp.Compare(b.weight, a.weight); c != 0 {
			return c
		}
		return cmp.Compare(a.key, b.key)
	})
	for i := 0; given < seats; i++ {
		out[rests[i].key]++
		given++
	}
	return out
}

// sortedTasks sorts tasks by id, in place, and returns them.
func sortedTasks(tasks []Task) []Task {
	slices.SortFunc(tasks, func(a, b Task) int { return cmp.Compare(a.ID, b.ID) })
	return tasks
}
