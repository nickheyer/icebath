package pypath

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
)

func TestErrnoNameMatchesErrorcode(t *testing.T) {
	if _, ok := ErrnoName(errors.New("plain")); ok {
		t.Fatal("named an error without an errno")
	}
	want := map[syscall.Errno]string{syscall.ENOSPC: "ENOSPC", syscall.EACCES: "EACCES", syscall.EROFS: "EROFS"}
	if runtime.GOOS == "windows" {
		want = map[syscall.Errno]string{112: "ENOSPC", 5: "EACCES", 19: "EACCES", 0x80070070: "ENOSPC", 10061: "WSAECONNREFUSED", 10013: "EACCES"}
	}
	for errno, name := range want {
		got, ok := ErrnoName(&os.PathError{Op: "open", Path: "x", Err: errno})
		if !ok || got != name {
			t.Errorf("%d = %q want %q", uint(errno), got, name)
		}
	}
}
