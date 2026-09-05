//go:build fixrate

package fixrate

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
