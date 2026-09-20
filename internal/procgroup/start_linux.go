//go:build linux

package procgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// bootIDPath holds a random id the kernel chooses at every boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// startTime returns pid's start time as "<boot id>/<clock ticks since boot>".
//
// The tick count alone repeats across reboots, so the boot id qualifies it: a
// process recorded before a reboot never matches one started after it.
func startTime(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return "", errNoProcess
	}
	if err != nil {
		return "", fmt.Errorf("procgroup: read stat of %d: %w", pid, err)
	}
	ticks, err := parseStatStartTime(string(data))
	if err != nil {
		return "", err
	}
	boot, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("procgroup: read boot id: %w", err)
	}
	return strings.TrimSpace(string(boot)) + "/" + ticks, nil
}
