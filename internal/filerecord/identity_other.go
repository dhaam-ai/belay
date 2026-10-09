//go:build !darwin && !linux

package filerecord

import "io/fs"

// identity reports nothing on a platform whose status-change time this
// package does not read, so every check hashes every file: slower, and
// exactly as correct.
func identity(fs.FileInfo) (uint64, int64) { return 0, 0 }
