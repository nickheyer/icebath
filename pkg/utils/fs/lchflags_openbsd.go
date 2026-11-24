package fsutil

import "golang.org/x/sys/unix"

// OpenBSD changes the flags of a symlink itself only through chflagsat, which x/sys/unix does not wrap and which the Go runtime answers with ENOSYS when made as a raw system call there, so the change is reported unsupported and skipped
func lchflags(string, uint32) error { return unix.EOPNOTSUPP }
