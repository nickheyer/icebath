// Advisory file locks shared between icebath processes
package lockfile

import (
	"errors"
	"os"
)

// Error of Try when another process holds the lock
var ErrHeld = errors.New("held by another process")

// Exclusive lock on a file held until Release
type Lock struct {
	f *os.File
}

// Opens or creates path and takes its lock, blocking until it is granted
func Acquire(path string) (*Lock, error) { return open(path, true) }

// Opens or creates path and takes its lock, failing with ErrHeld when another process holds it
func Try(path string) (*Lock, error) { return open(path, false) }

func open(path string, block bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f, block); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Reports whether the lock of path is held through any other descriptor, in this process or another, false when the
// file does not exist, without creating it
func Held(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	err = lock(f, false)
	if errors.Is(err, ErrHeld) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, unlock(f)
}

// Releases the lock and closes the file, leaving it in place
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
