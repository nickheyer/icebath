package proc

import (
	"errors"
	"os"
	"slices"
	"syscall"
)

// Raised when the OS refuses to show another process's descriptors, psutil AccessDenied
var ErrAccessDenied = errors.New("access denied")

// One regular file a process holds open, as psutil open_files reports it
type OpenFile struct {
	Path string
	FD   int
}

// Every live pid in ascending order like psutil pids
func PIDs() ([]int, error) {
	pids, err := listPIDs()
	if err != nil {
		return nil, err
	}
	slices.Sort(pids)
	return pids, nil
}

// psutil isfile_strict, a permission failure raised and any other stat failure reading as false
func isFileStrict(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			return false, err
		}
		return false, nil
	}
	return fi.Mode().IsRegular(), nil
}
