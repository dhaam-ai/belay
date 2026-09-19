//go:build unix

package procgroup

import (
	"fmt"
	"strconv"
	"strings"
)

// statStartField is the position of starttime among the fields that follow
// the command name in a /proc/<pid>/stat line. proc(5) numbers starttime 22
// and the state, the first field after the command, 3.
const statStartField = 22 - 3

// parseStatStartTime returns the starttime field of a /proc/<pid>/stat line:
// the process's start time, in clock ticks since boot, as the decimal string
// the kernel wrote.
//
// The command name, the second field, is in parentheses and may itself hold
// spaces and parentheses, so fields are counted from the last ')'. It lives
// apart from the Linux-only reader so that its tests run on every platform.
func parseStatStartTime(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("procgroup: stat line has no command name: %q", stat)
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) <= statStartField {
		return "", fmt.Errorf("procgroup: stat line has %d fields after the command, want more than %d",
			len(fields), statStartField)
	}
	start := fields[statStartField]
	if _, err := strconv.ParseUint(start, 10, 64); err != nil {
		return "", fmt.Errorf("procgroup: stat starttime %q is not a count of clock ticks: %w", start, err)
	}
	return start, nil
}
