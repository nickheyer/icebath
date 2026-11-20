//go:build !windows

package backup

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestIsZeroedNeverProbesSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "state.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if state.IsZeroedStateDB(fifo, 100, false) {
		t.Fatal("fifo classified as zeroed")
	}
}
