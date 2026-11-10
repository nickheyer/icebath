//go:build !linux

package app

import (
	"os"
	"testing"
)

// Status change time of a file in nanoseconds, the modification time where the platform keeps no change time
func statCtimeNs(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime().UnixNano()
}
