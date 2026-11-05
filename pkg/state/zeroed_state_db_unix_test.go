//go:build unix

package state

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestIsZeroedNeverProbesSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "state.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	verdict := make(chan bool, 1)
	go func() { verdict <- IsZeroedStateDB(fifo, 100, false) }()
	select {
	case zeroed := <-verdict:
		if zeroed {
			t.Fatal("a FIFO is never zeroed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the zeroed probe blocked opening a FIFO")
	}
}
