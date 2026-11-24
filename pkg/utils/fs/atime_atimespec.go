//go:build darwin || freebsd || netbsd

package fsutil

import (
	"io/fs"
	"syscall"
	"time"
)

// Access time of a file from its stat result to the nanosecond, the modification time when the result carries none
func Atime(fi fs.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(int64(st.Atimespec.Sec), int64(st.Atimespec.Nsec))
}
