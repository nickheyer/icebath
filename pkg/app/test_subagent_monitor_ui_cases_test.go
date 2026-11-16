package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/term"
)

// Monitor application on a terminal emulator fed by a pipe of typed bytes
type monitorHarness struct {
	t     *testing.T
	app   *monitorApp
	emu   *term.Emulator
	keys  *io.PipeWriter
	wrote sync.Mutex
	done  chan struct{}
}

// Writer feeding the monitor harness emulator with newlines returning the carriage
type monitorEmulatorWriter struct{ h *monitorHarness }

func (w monitorEmulatorWriter) Write(p []byte) (int, error) {
	w.h.wrote.Lock()
	defer w.h.wrote.Unlock()
	if _, err := w.h.emu.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(string(p), "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Types raw terminal bytes
func (h *monitorHarness) typeBytes(s string) {
	h.t.Helper()
	if _, err := io.WriteString(h.keys, s); err != nil {
		h.t.Fatalf("typing %q: %v", s, err)
	}
}

// Visible emulator rows with trailing blanks trimmed
func (h *monitorHarness) screen() []string {
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

// Whether some visible row contains the text
func (h *monitorHarness) shows(text string) bool {
	for _, row := range h.screen() {
		if strings.Contains(row, text) {
			return true
		}
	}
	return false
}

// Waits until some visible row contains the text
func (h *monitorHarness) waitShows(text string) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !h.shows(text) {
		if time.Now().After(deadline) {
			h.t.Fatalf("%q never appeared, screen:\n%s", text, strings.Join(h.screen(), "\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Whether the monitor run has returned
func (h *monitorHarness) exited(within time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(within):
		return false
	}
}

// Notice line text under the application lock
func (h *monitorHarness) notice() string {
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	return h.app.notice
}

// Child double whose steer text and stop request are recorded
type pinnedChild struct {
	mu       sync.Mutex
	received []string
}

func (c *pinnedChild) Steer(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, text)
	return true
}

func (c *pinnedChild) Interrupt(string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received = append(c.received, "STOP")
}

func (c *pinnedChild) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.received...)
}

func TestExtendedTailIsBoundedAndLiteral(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := bareCLI()
	dir := t.TempDir()
	path := filepath.Join(dir, "child.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old data\n", 20000)+"\x1b[2Jlast activity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := readTail(c, path)
	if !strings.Contains(tail, "last activity") || strings.Contains(tail, "\x1b") {
		t.Fatalf("tail %q", tail[max(0, len(tail)-80):])
	}
	if utf8.RuneCountInString(tail) > 32768 {
		t.Fatalf("tail is %d characters", utf8.RuneCountInString(tail))
	}
	if !strings.Contains(readTail(c, filepath.Join(dir, "missing.log")), "not available") {
		t.Fatal("missing transcript is not reported unavailable")
	}
}
