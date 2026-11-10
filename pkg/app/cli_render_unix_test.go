//go:build unix

package app

import (
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Replaces the termios seams with no ops so a pipe can stand in for the terminal
func fakeTermios(t *testing.T) {
	t.Helper()
	prevGet, prevSet := termiosGet, termiosSet
	termiosGet = func(int) (*unix.Termios, error) { return &unix.Termios{}, nil }
	termiosSet = func(int, uint, *unix.Termios) error { return nil }
	t.Cleanup(func() { termiosGet, termiosSet = prevGet, prevSet })
}

// A pipe standing in for the terminal, the read end installed as the console input
func osc11Pipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	clearLightEnv(t)
	fakeTermios(t)
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		readEnd.Close()
		writeEnd.Close()
	})
	installTTYConsole(t, readEnd)
	return readEnd, writeEnd
}

func TestOsc11DrainGuardLateReplyIsConsumedNotLeaked(t *testing.T) {
	readEnd, writeEnd := osc11Pipe(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = writeEnd.Write([]byte("\x1b]11;rgb:0c0c/0c0c/0c0c\x1b\\\x1b[?62;22c"))
	}()
	bg, ok := queryOSC11Background()
	if !ok || bg != "#0C0C0C" {
		t.Fatalf("got %q %v", bg, ok)
	}
	if fdReadable(int(readEnd.Fd()), time.Now()) {
		t.Fatal("late OSC 11 bytes must be consumed, not left to leak")
	}
}

func TestOsc11DrainGuardPostDeadlineStragglerIsDrained(t *testing.T) {
	readEnd, writeEnd := osc11Pipe(t)
	_, _ = writeEnd.Write([]byte("\x1b[?62;22c"))
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = writeEnd.Write([]byte("\x1b]11;rgb:0c0c/0c0c/0c0c\x1b\\"))
	}()
	bg, ok := queryOSC11Background()
	if ok || bg != "" {
		t.Fatalf("OSC 11 was swallowed, only DA1 answered, got %q", bg)
	}
	if fdReadable(int(readEnd.Fd()), time.Now()) {
		t.Fatal("the drain loop should have consumed the straggler bytes")
	}
}
