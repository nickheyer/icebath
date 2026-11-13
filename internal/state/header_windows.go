package state

import (
	"errors"
	"io"
	"os"
)

// Windows holds no POSIX locks a close could cancel, so probes need no cached descriptors
type headerProbes struct{}

// Empty probe state
func newHeaderProbes() headerProbes { return headerProbes{} }

// Up to n bytes of path at off read through a short lived handle
func (e *Engine) preadDB(path string, off int64, n int) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return buf[:got], true
}
