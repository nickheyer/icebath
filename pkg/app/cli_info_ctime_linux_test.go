package app

import (
	"os"
	"syscall"
	"testing"
)

// Status change time of a file in nanoseconds
func statCtimeNs(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ctim.Nano()
}
