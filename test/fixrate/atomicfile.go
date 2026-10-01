//go:build fixrate

package fixrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// This file is how the corpus writes anything a resumed run will believe.
//
// A long corpus run is interrupted -- by a person, a reboot, a kill -9 -- and
// the next invocation decides what to skip by what it finds on disk. A file
// that a crash left half-written would be read as a finished cell, or as a
// verified dataset, and the run would carry a wrong fact forward forever. So
// nothing is written in place: each file is written beside its destination,
// flushed, renamed over it, and the directory flushed in turn, the same
// discipline belay's own journal and state files keep (ADR-0003). A reader
// therefore sees the old file or the whole new one, never part of either.

// createTemp creates the temporary file a write to path goes through, in
// path's own directory so the final rename cannot cross a filesystem. Its
// name starts with a dot, so nothing that lists the directory for results
// mistakes it for one.
func createTemp(path string) (*os.File, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("fixrate: create a temporary file for %s: %w", path, err)
	}
	return tmp, nil
}

// commitTemp flushes tmp, renames it over path, and flushes path's
// directory, so the rename itself survives a crash.
func commitTemp(tmp *os.File, path string) error {
	if err := tmp.Sync(); err != nil {
		return discardTemp(tmp, fmt.Errorf("fixrate: flush %s: %w", path, err))
	}
	if err := tmp.Close(); err != nil {
		return discardTemp(nil, errors.Join(fmt.Errorf("fixrate: close %s: %w", path, err), os.Remove(tmp.Name())))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("fixrate: rename into %s: %w", path, err), os.Remove(tmp.Name()))
	}
	return syncDir(filepath.Dir(path))
}

// discardTemp closes and removes tmp, if there is one, and returns cause
// together with anything that went wrong doing so.
func discardTemp(tmp *os.File, cause error) error {
	if tmp == nil {
		return cause
	}
	return errors.Join(cause, tmp.Close(), os.Remove(tmp.Name()))
}

// syncDir flushes dir, which is what makes a rename into it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // a directory this package itself writes into
	if err != nil {
		return fmt.Errorf("fixrate: open %s to flush it: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		return errors.Join(fmt.Errorf("fixrate: flush %s: %w", dir, err), d.Close())
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("fixrate: close %s: %w", dir, err)
	}
	return nil
}
