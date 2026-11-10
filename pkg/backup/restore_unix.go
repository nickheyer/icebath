//go:build !windows

package backup

import (
	"os"
	"syscall"
)

// Hard link count of the file path names, following a symlink
func linkCount(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return uint64(st.Nlink), nil
}

// Full st_mode of a stat result
func statMode(st os.FileInfo) uint32 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint32(s.Mode)
	}
	return uint32(st.Mode().Perm())
}

// chmod with every permission bit of an st_mode, setuid and setgid included
func chmodFull(path string, mode uint32) error {
	if err := syscall.Chmod(path, mode&0o7777); err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}

// Applies the publish mode to an open staged file before any byte is written
func fchmodStaged(f *os.File, name string, mode uint32) error {
	if err := syscall.Fchmod(int(f.Fd()), mode&0o7777); err != nil {
		return &os.PathError{Op: "fchmod", Path: name, Err: err}
	}
	return nil
}

// Mode open(path, "wb") gives a new file under the process umask, read with a restrictive probe mask
func processNewFileMode() (uint32, bool) {
	current := syscall.Umask(0o077)
	syscall.Umask(current)
	return uint32(0o666 &^ current), true
}
