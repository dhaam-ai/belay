//go:build unix && !linux

package procgroup

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
)

// psPaths are where ps is looked for. It is never looked up on PATH: this
// runs while belay is starting an agent or a test suite, whose PATH may be
// anything.
var psPaths = []string{"/bin/ps", "/usr/bin/ps"}

// startTime returns what `ps -o lstart=` prints for pid, in the C locale and
// UTC, so that neither the user's locale nor a daylight-saving change can
// make one process's start time read differently later.
//
// The process is started with os/exec directly, not internal/exec: it is part
// of recording a process, and must not be recorded itself.
func startTime(pid int) (string, error) {
	ps, err := findPS()
	if err != nil {
		return "", err
	}
	// #nosec G204 -- ps is one of the fixed psPaths and every argument is a
	// constant or a formatted integer; no shell is involved.
	cmd := osexec.Command(ps, "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C", "TZ=UTC0"}
	out, err := cmd.Output()
	start := strings.TrimSpace(string(out))
	var exit *osexec.ExitError
	switch {
	case errors.As(err, &exit) && start == "":
		// ps exits 1, printing nothing, when no process has the pid.
		return "", errNoProcess
	case err != nil:
		return "", fmt.Errorf("procgroup: %s -o lstart= -p %d: %w", ps, pid, err)
	case start == "":
		return "", errNoProcess
	}
	return start, nil
}

// commandLine returns what `ps -ww -o command=` prints for pid: the
// process's arguments as it now reports them, joined by spaces, uncut.
func commandLine(pid int) (string, error) {
	ps, err := findPS()
	if err != nil {
		return "", err
	}
	// #nosec G204 -- ps is one of the fixed psPaths and every argument is a
	// constant or a formatted integer; no shell is involved.
	cmd := osexec.Command(ps, "-ww", "-o", "command=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"LC_ALL=C"}
	out, err := cmd.Output()
	line := strings.TrimSpace(string(out))
	var exit *osexec.ExitError
	switch {
	case errors.As(err, &exit) && line == "":
		return "", errNoProcess
	case err != nil:
		return "", fmt.Errorf("procgroup: %s -ww -o command= -p %d: %w", ps, pid, err)
	}
	return line, nil
}

// findPS returns the first of psPaths that exists.
func findPS() (string, error) {
	for _, p := range psPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("procgroup: ps not found at %s", strings.Join(psPaths, " or "))
}
