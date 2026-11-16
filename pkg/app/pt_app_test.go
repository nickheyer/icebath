package app

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/term"
)

// ptHarness runs a prompt application against a terminal emulator and a pipe of typed bytes
type ptHarness struct {
	t     *testing.T
	app   *ptApplication
	emu   *term.Emulator
	keys  *io.PipeWriter
	done  chan error
	once  sync.Once
	wrote sync.Mutex
}

// Writer feeding the emulator, serialized with screen reads
type emulatorWriter struct{ h *ptHarness }

// Writes to the emulator the way a pty line discipline does, a newline also returning the carriage
func (w emulatorWriter) Write(p []byte) (int, error) {
	w.h.wrote.Lock()
	defer w.h.wrote.Unlock()
	if _, err := w.h.emu.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Types raw terminal bytes
func (h *ptHarness) typeBytes(s string) {
	h.t.Helper()
	if _, err := io.WriteString(h.keys, s); err != nil {
		h.t.Fatalf("typing %q: %v", s, err)
	}
}

// Visible emulator rows, trailing blanks trimmed
func (h *ptHarness) screen() []string {
	h.wrote.Lock()
	defer h.wrote.Unlock()
	_, rows := h.emu.Size()
	out := make([]string, rows)
	for y := 0; y < rows; y++ {
		line, _ := h.emu.Row(h.emu.Base() + uint64(y))
		var b strings.Builder
		for _, cell := range line.Cells {
			b.WriteString(cell.Content)
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// Reports whether some visible row contains the text
func (h *ptHarness) shows(text string) bool {
	for _, row := range h.screen() {
		if strings.Contains(row, text) {
			return true
		}
	}
	return false
}

// Waits until some visible row contains the text
func (h *ptHarness) waitShows(text string) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.shows(text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("%q never appeared, screen:\n%s", text, strings.Join(h.screen(), "\n"))
}

// Runs fn on the application loop and waits for it
func (h *ptHarness) onLoop(fn func()) {
	h.t.Helper()
	ran := make(chan struct{})
	if !h.app.CallSoonThreadsafe(func() { fn(); close(ran) }) {
		h.t.Fatal("application is not running")
	}
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		h.t.Fatal("loop callback never ran")
	}
}

// Waits for the program to exit and returns its error
func (h *ptHarness) wait() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatalf("application never exited, screen:\n%s", strings.Join(h.screen(), "\n"))
		return nil
	}
}

// Ends the program if it still runs
func (h *ptHarness) stop() {
	h.once.Do(func() {
		if h.app.IsRunning() {
			h.app.Exit()
			select {
			case <-h.done:
			case <-time.After(5 * time.Second):
			}
		}
		_ = h.keys.Close()
	})
}
