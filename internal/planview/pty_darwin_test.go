//go:build darwin

package planview

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair: the viewer is given the slave, as it
// would be given the terminal a person types into, and the test reads what
// it draws from the master and types into it. It skips where the system
// has no pseudo-terminals to give, such as a minimal container.
//
// The slave is named from the master's minor device number, which is how
// the kernel numbers the pair (/dev/ttysNNN). x/sys/unix has no typed
// wrapper for TIOCPTYGNAME, the ioctl libc's ptsname uses, and the raw
// system call is deprecated on darwin.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available here (%v); the viewer's terminal handling is not exercised", err)
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatalf("grant the pseudo-terminal: %v", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatalf("unlock the pseudo-terminal: %v", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("stat the pseudo-terminal: %v", err)
	}
	path := fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(st.Rdev))) //nolint:gosec // G115: a device number is not negative
	s, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)        //nolint:gosec // G304: the kernel numbered it
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if !term.IsTerminal(s.Fd()) {
		t.Fatalf("%s is not a terminal, so it is not this pseudo-terminal's slave", path)
	}
	return m, s
}
