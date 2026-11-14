//go:build linux

package tools

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Makes a named pipe at the path
func ftrMkfifo(t *testing.T, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileToolsReadSpecialFileKindFifo(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "p.pipe")
	ftrMkfifo(t, fifo)
	if k := specialFileKind(fifo); !strings.Contains(k, "FIFO") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if k := specialFileKind(sockPath); !strings.Contains(k, "socket") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadSpecialFileKindSymlinkToFifoFollowed(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "p.pipe")
	ftrMkfifo(t, fifo)
	link := filepath.Join(dir, "innocent.txt")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	if k := specialFileKind(link); !strings.Contains(k, "FIFO") {
		t.Fatalf("kind %q", k)
	}
}

func TestFileToolsReadFifoReadReturnsNoteInstantly(t *testing.T) {
	env, ws := newFileToolTestEnv(t)
	fifo := filepath.Join(ws, "live.pipe")
	ftrMkfifo(t, fifo)
	done := make(chan string, 1)
	go func() { done <- readFileTool(context.Background(), env, fifo, nil, nil, "default") }()
	var raw string
	select {
	case raw = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("guard must not block on the FIFO")
	}
	r := ftDecode(t, raw)
	if r["success"] != false {
		t.Fatalf("result %v", r)
	}
	if note, _ := r["note"].(string); !strings.Contains(note, "FIFO") {
		t.Fatalf("result %v", r)
	}
}
