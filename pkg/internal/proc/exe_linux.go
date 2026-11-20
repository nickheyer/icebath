//go:build linux

package proc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Executable path of pid, empty for a live kernel thread without one
func Exe(pid int) (string, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	target, err := os.Readlink(filepath.Join(dir, "exe"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := os.Lstat(dir); lerr == nil {
				return "", nil
			}
		}
		return "", err
	}
	target, _, _ = strings.Cut(target, "\x00")
	if strings.HasSuffix(target, " (deleted)") {
		if _, err := os.Stat(target); err != nil {
			target = strings.TrimSuffix(target, " (deleted)")
		}
	}
	return target, nil
}
