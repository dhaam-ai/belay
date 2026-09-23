//go:build linux

package planview

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair: the viewer is given the slave, as it
// would be given the terminal a person types into, and the test reads what
// it draws from the master and types into it. It skips where the system
// has no pseudo-terminals to give, such as a minimal container.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available here (%v); the viewer's terminal handling is not exercised", err)
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock the pseudo-terminal: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("number the pseudo-terminal: %v", err)
	}
	path := fmt.Sprintf("/dev/pts/%d", n)
	s, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0) //nolint:gosec // G304: the kernel numbered it
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return m, s
}
