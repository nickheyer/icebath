package pypath

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
)

// True for the errors pathlib exists and is_dir turn into False, ENOENT ENOTDIR EBADF ELOOP and three winerrors
func PathlibIgnoredError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if runtime.GOOS == "windows" && (errno == 21 || errno == 123 || errno == 1921) {
		return true
	}
	switch name, _ := ErrnoName(err); name {
	case "ENOENT", "ENOTDIR", "EBADF", "ELOOP":
		return true
	}
	return false
}

// Followed stat of pathlib exists, is_file and is_dir, nil info without error where those report False
func PathlibStat(p string) (os.FileInfo, error) {
	if strings.ContainsRune(p, 0) {
		return nil, nil
	}
	fi, err := os.Stat(p)
	if err != nil {
		if PathlibIgnoredError(err) {
			return nil, nil
		}
		return nil, err
	}
	return fi, nil
}
