package pm

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Owner uid of a stat result, zero as CPython reports on Windows
func fileOwnerUID(os.FileInfo) int { return 0 }

// Reports whether pid is alive
func stillRunning(pid int) bool { return proc.Exists(pid) }

// Reports the unlink refusals hermes reads as PermissionError, access denied and a sharing violation
func unlinkPermissionDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.Errno(32))
}
