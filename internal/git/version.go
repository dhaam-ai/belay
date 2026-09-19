//go:build unix

package git

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dhaam-ai/belay/pkg/belay"
)

// Version-related sentinels. Both are toolchain problems rather than answers
// about a repository: installing or upgrading git fixes either one, so a
// caller must not report them as "this repository cannot be used".
var (
	// ErrTooOld reports a git older than MinVersion. Recover the version
	// that was found from the error text; the number belongs in the message
	// a user reads, not in control flow.
	ErrTooOld = fmt.Errorf("%w: git is older than belay needs", belay.ErrToolchainMissing)

	// ErrUnreadableVersion reports that `git version` printed something this
	// package cannot read. A binary on PATH named git that does not print a
	// git version is not git, and guessing that it is new enough would be
	// the wrong way to be wrong.
	ErrUnreadableVersion = fmt.Errorf("%w: cannot read the git version", belay.ErrToolchainMissing)
)

// MinVersion is the oldest git this package works with. See the package doc
// for why the floor is 2.38 and not something older.
var MinVersion = Version{Major: 2, Minor: 38}

// Version is a git release as three numbers.
//
// Only the first three components are kept. Everything vendors and
// distributions append after them — "(Apple Git-157)", ".windows.1", ".rc2" —
// is packaging, not a release belay could branch on.
type Version struct {
	// Major is the first component, 2 for every git in use today.
	Major int
	// Minor is the second component, which is where git's real releases move.
	Minor int
	// Patch is the third component, or 0 when git printed only two.
	Patch int
}

// String renders v as "major.minor.patch", so a version in an error message
// can be compared with what `git --version` prints.
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// AtLeast reports whether v is floor or newer, comparing major, then minor,
// then patch.
func (v Version) AtLeast(floor Version) bool {
	switch {
	case v.Major != floor.Major:
		return v.Major > floor.Major
	case v.Minor != floor.Minor:
		return v.Minor > floor.Minor
	default:
		return v.Patch >= floor.Patch
	}
}

// versionPrefix is what every git build prints before its numbers.
const versionPrefix = "git version "

// ParseVersion reads the line `git version` prints.
//
// The line is not a semantic version and never has been: macOS prints
// "git version 2.54.0 (Apple Git-157)", Windows prints
// "git version 2.50.1.windows.1", and a release candidate prints
// "git version 2.39.0.rc2". So parsing takes the leading numeric components
// of the first field and stops at the first one that is not a number, which
// keeps every one of those shapes readable while still refusing output that
// is not a git version at all.
//
// It returns an error wrapping ErrUnreadableVersion when the prefix is
// missing or fewer than two numeric components follow it. Two is the floor
// because a caller compares minors: a version with no minor could not be
// compared with MinVersion at all.
func ParseVersion(out string) (Version, error) {
	line := strings.TrimSpace(out)
	if _, found := strings.CutPrefix(line, versionPrefix); !found {
		return Version{}, fmt.Errorf("%w: %q does not start with %q", ErrUnreadableVersion, line, versionPrefix)
	}
	rest := strings.TrimPrefix(line, versionPrefix)
	field := strings.TrimSpace(rest)
	if i := strings.IndexAny(field, " \t"); i >= 0 {
		field = field[:i]
	}

	nums := make([]int, 0, 3)
	for _, part := range strings.Split(field, ".") {
		n, ok := digits(part)
		if !ok || len(nums) == 3 {
			break
		}
		nums = append(nums, n)
	}
	if len(nums) < 2 {
		return Version{}, fmt.Errorf("%w: %q has no major.minor", ErrUnreadableVersion, line)
	}
	v := Version{Major: nums[0], Minor: nums[1]}
	if len(nums) == 3 {
		v.Patch = nums[2]
	}
	return v, nil
}

// digits parses s as a non-negative decimal number, rejecting anything that
// is not made only of ASCII digits. strconv alone would accept "-1" and
// "+2", neither of which git prints, and a negative component would compare
// as older than every real release.
func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
