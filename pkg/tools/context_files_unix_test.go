//go:build unix

package tools

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSlowFileTimesOutAndReadErrorsPropagate(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "slow.md")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	timeout := 0.2
	start := time.Now()
	text, ok, err := ReadTextWithTimeout(nil, fifo, &timeout)
	if ok || err != nil || text != "" || time.Since(start) > 3*time.Second {
		t.Fatalf("slow read returned %q %v %v", text, ok, err)
	}
	opener, oerr := os.OpenFile(fifo, os.O_WRONLY, 0)
	if oerr == nil {
		opener.Close()
	}
	if _, _, err := ReadTextWithTimeout(nil, dir, &timeout); err == nil {
		t.Fatal("reading a directory did not raise")
	}
}
