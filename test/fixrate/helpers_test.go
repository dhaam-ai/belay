//go:build fixrate

package fixrate

import "github.com/dhaam-ai/belay/pkg/belay"

// equalStrings reports whether got and want hold the same strings in the
// same order.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] { //nolint:gosec // i ranges over got, whose length was just compared equal to want's above
			return false
		}
	}
	return true
}

// belayUsageForTest builds a belay.Usage with just the two fields the
// paired report reads, so a test's intent is not buried in a struct
// literal full of zeroes.
func belayUsageForTest(usd float64, estimated bool) belay.Usage {
	return belay.Usage{USD: usd, Estimated: estimated}
}
