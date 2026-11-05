package pm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// WaitForever makes a lock wait without a deadline
const WaitForever time.Duration = -1

const lockPollInterval = 50 * time.Millisecond

const (
	verbatimPrefix    = `\\?\`
	verbatimUNCPrefix = `\\?\UNC\`
)

// Reports a Windows mount point reparse tag, false for anything that cannot be inspected or on POSIX
func IsJunction(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return reparseIsMountPoint(path)
}

// Absolute backslashed path with the extended length prefix applied once
func longForm(text string) string {
	if strings.HasPrefix(text, verbatimPrefix) {
		return text
	}
	if abs, err := filepath.Abs(text); err == nil {
		text = abs
	}
	text = strings.ReplaceAll(text, "/", `\`)
	if strings.HasPrefix(text, `\\`) {
		return verbatimUNCPrefix + text[2:]
	}
	return verbatimPrefix + text
}

// Spells a PM owned root in extended length form on Windows so deep paths beneath it survive MAX_PATH
func LongRoot(path string) string {
	if runtime.GOOS == "windows" {
		return longForm(path)
	}
	return path
}

// Ordinary spelling of a path for argv, environment values, records and messages
func Native(path string) string {
	if strings.HasPrefix(path, verbatimUNCPrefix) {
		return `\\` + path[len(verbatimUNCPrefix):]
	}
	return strings.TrimPrefix(path, verbatimPrefix)
}

// Takes the exclusive byte lock, polling until the timeout, WaitForever never gives up and zero tries once
func LockFD(f *os.File, wait bool, timeout time.Duration) (bool, error) {
	var deadline time.Time
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		held, err := tryLockFile(f)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
		if !wait || (timeout >= 0 && !time.Now().Before(deadline)) {
			return false, nil
		}
		time.Sleep(lockPollInterval)
	}
}

// Releases the byte lock ignoring failures since closing the file releases it as well
func UnlockFD(f *os.File) {
	_ = unlockFile(f)
}

// Opens or creates path and returns it with its lock held, nil when the wait expired
func LockPath(path string, timeout time.Duration, createDir bool) (*os.File, error) {
	var deadline time.Time
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	expired := func() bool { return timeout >= 0 && !time.Now().Before(deadline) }
	for {
		if createDir {
			if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
				return nil, err
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if st, serr := os.Stat(filepath.Dir(path)); !createDir || (serr == nil && st.IsDir()) {
				return nil, err
			}
			if expired() {
				return nil, nil
			}
			continue
		}
		remaining := WaitForever
		if timeout >= 0 {
			remaining = max(0, time.Until(deadline))
		}
		held, err := LockFD(f, true, remaining)
		if err != nil {
			f.Close()
			return nil, err
		}
		if !held {
			f.Close()
			return nil, nil
		}
		if runtime.GOOS == "windows" || sameFile(f, path) {
			return f, nil
		}
		f.Close()
		if expired() {
			return nil, nil
		}
	}
}

// Reports whether the open file is still the file currently at path
func sameFile(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	current, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, current)
}

// File bytes with ok false when the file does not exist, other read failures returned as errors
func ReadBytesOrNone(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Hex sha256 of a file with ok false when it does not exist
func FileDigest(path string) (string, bool, error) {
	data, ok, err := ReadBytesOrNone(path)
	if err != nil || !ok {
		return "", ok, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true, nil
}

// Replaces path atomically and fsyncs file and directory so a crash leaves either old or new bytes
func DurableWriteBytes(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".publish-")
	if err != nil {
		return err
	}
	temporary := tmp.Name()
	defer os.Remove(temporary)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameFile(temporary, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}

// Delays between retries of a rename like mutation that a transient Windows hold refused
var heldRetryDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// Runs a rename like mutation and rides out transient Windows holds for about three seconds
func RetryHeld(operation func() error) error {
	for i := 0; ; i++ {
		err := operation()
		if err == nil {
			return nil
		}
		if i >= len(heldRetryDelays) || !isHeldError(err) {
			return err
		}
		time.Sleep(heldRetryDelays[i])
	}
}

// Removes a tree, refusing a missing path, a symlink or a junction the way shutil rmtree does
func RemoveTree(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 || IsJunction(path) {
		return &fs.PathError{Op: "rmtree", Path: path, Err: errors.New("cannot call rmtree on a symbolic link")}
	}
	if !st.IsDir() {
		return &fs.PathError{Op: "rmtree", Path: path, Err: fmt.Errorf("not a directory")}
	}
	return os.RemoveAll(path)
}

// Removes a tree ignoring every failure and leaving a symlink or junction at path untouched
func removeTreeQuiet(path string) {
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&fs.ModeSymlink != 0 || IsJunction(path) || !st.IsDir() {
		return
	}
	_ = os.RemoveAll(path)
}
