//go:build !windows

package app

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// os.ttyname of a file descriptor, an error when the descriptor is not a terminal
func ttyNameOS(fd int) (string, error) {
	if _, err := unix.IoctlGetTermios(fd, ioctlReadTermios); err != nil {
		return "", err
	}
	if link, err := os.Readlink(filepath.Join("/proc/self/fd", strconv.Itoa(fd))); err == nil && link != "" {
		return link, nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	for _, dir := range []string{"/dev/pts", "/dev"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			var cand unix.Stat_t
			if err := unix.Stat(p, &cand); err == nil && cand.Rdev == st.Rdev && cand.Ino == st.Ino {
				return p, nil
			}
		}
	}
	return "", errors.New("ttyname: device not found")
}
