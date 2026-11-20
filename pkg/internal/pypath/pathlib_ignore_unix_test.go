//go:build !windows

package pypath

import (
	"io/fs"
	"syscall"
	"testing"
)

func TestPathlibIgnoredErrorPosixErrnos(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EBADF, syscall.ELOOP} {
		if !PathlibIgnoredError(&fs.PathError{Op: "stat", Path: "p", Err: errno}) {
			t.Errorf("%v not ignored", errno)
		}
	}
}
