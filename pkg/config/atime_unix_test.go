//go:build !windows

package config

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Access time the file system records for path
func fileAccessTime(t *testing.T, path string) time.Time {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
}
