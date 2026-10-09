//go:build linux

package filerecord

import (
	"io/fs"
	"syscall"
)

// identity returns info's inode number and status-change time in
// nanoseconds, or zeros when the platform does not say.
func identity(info fs.FileInfo) (uint64, int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return st.Ino, st.Ctim.Nano()
}
