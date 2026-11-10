//go:build !windows

package selfupdate

import (
	"os"
	"syscall"
)

// Owner uid of a path without following a final link
func defaultLstatOwner(path string) (int, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return int(st.Sys().(*syscall.Stat_t).Uid), nil
}
