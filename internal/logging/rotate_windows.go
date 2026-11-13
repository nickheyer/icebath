//go:build windows

package logging

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"time"
)

// Line ending the log files are written with, as Windows text files carry it
const lineEnding = "\r\n"

// Characters an export file name never takes from a session id, both separators and the drive and stream marker here
const nameSeparators = `/\:`

// What a Windows write does once the lock file cannot be locked
const lockFallback = "file logging fell back to appends without rollover."

// Windows log state, the switch to unlocked appends once locking proved impossible
type platformFile struct {
	unlocked bool
}

// Delays a Windows rename waits through while a scanner or reader briefly holds the file
func renameDelays() []time.Duration {
	return []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}
}

// Leaves the log unopened, every write opening and closing it under the lock so a rename by another process works
func (f *rotatingFile) prepare() error { return nil }

// Windows holds no stream between writes
func (f *rotatingFile) dropStream() {}

// Switches the log to unlocked appends that never roll over, since a rename would race the other writers
func (f *rotatingFile) lockUnavailable() bool {
	f.unlocked = true
	return false
}

// Appends one line under the lock, rotating first when the log already holds the size limit
func (f *rotatingFile) write(line string) {
	if f.closed {
		return
	}
	f.skipped = false
	if f.skipMissingDir && !pathExists(f.path) && !isDir(f.dir) {
		f.skipped = true
		return
	}
	rotate := !f.unlocked
	if rotate {
		if err := f.lock(); err == nil {
			defer f.unlock()
		} else {
			rotate = f.lockFailed(err)
		}
	}
	if f.skipMissingDir && !isDir(f.dir) {
		f.skipped = true
		return
	}
	if err := f.emit(line, rotate); err != nil {
		f.fail(line, err)
	}
}

// Rotates a log at or past the size limit when allowed, then opens it, appends the line and closes it
func (f *rotatingFile) emit(line string, rotate bool) error {
	if rotate && f.maxBytes > 0 {
		if st, err := os.Stat(f.path); err == nil && float64(st.Size()) >= f.maxBytes {
			if err := f.rollover(); err != nil {
				f.rolloverFailed(err)
			}
		}
	}
	out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	f.chmodManaged()
	_, werr := out.WriteString(strings.ReplaceAll(line+"\n", "\n", lineEnding))
	return errors.Join(werr, out.Close())
}

// Truncates a log without backups, else renames it aside, shifts the backups and makes it the first
func (f *rotatingFile) rollover() error {
	if f.backups <= 0 {
		out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		if err != nil {
			return err
		}
		f.chmodManaged()
		return out.Close()
	}
	tmp := ""
	for tmp == "" || pathExists(tmp) {
		tmp = fmt.Sprintf("%s.rotate.%08d", f.path, rand.IntN(100000000))
	}
	if err := os.Rename(f.path, tmp); err != nil {
		return err
	}
	for i := f.backups - 1; i > 0; i-- {
		if src := backupName(f.path, i); pathExists(src) {
			if err := moveOver(src, backupName(f.path, i+1)); err != nil {
				return errors.Join(err, f.restore(tmp))
			}
		}
	}
	if err := moveOver(tmp, backupName(f.path, 1)); err != nil {
		return errors.Join(err, f.restore(tmp))
	}
	return nil
}

// Puts a log set aside by an unfinished rotation back at its path, followed by lines an unlocked writer added
func (f *rotatingFile) restore(tmp string) error {
	if pathExists(f.path) {
		newer, err := os.ReadFile(f.path)
		if err == nil {
			err = os.Remove(f.path)
		}
		if err != nil {
			return fmt.Errorf("records left in %s: %w", tmp, err)
		}
		out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return errors.Join(fmt.Errorf("records left in %s: %w", tmp, err), os.WriteFile(f.path, newer, 0o666))
		}
		_, werr := out.Write(newer)
		if err := errors.Join(werr, out.Close()); err != nil {
			return fmt.Errorf("records left in %s: %w", tmp, err)
		}
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("records left in %s: %w", tmp, err)
	}
	return nil
}
