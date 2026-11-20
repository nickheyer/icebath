//go:build linux || openbsd || dragonfly || solaris || illumos

package pyshutil

import (
	"os"
	"syscall"
	"time"
)

// Access time of a file from its stat result
func fileAtime(fi os.FileInfo) time.Time {
	st := fi.Sys().(*syscall.Stat_t)
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
}
