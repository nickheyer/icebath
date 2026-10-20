// Advisory file locks so several processes can share a session directory
package flock

import "os"

// Exclusive lock on an open file held until Unlock
type Lock struct {
	f *os.File
}

// Opens or creates path and takes an exclusive lock, blocking until it is granted
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Releases the lock and closes the file
func (l *Lock) Unlock() error {
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
