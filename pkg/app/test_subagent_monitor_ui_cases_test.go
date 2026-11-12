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

	"github.com/nickheyer/icebath/pkg/agent"
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

// Builds the monitor application on a cols by rows emulator without starting it
func newMonitorHarness(t *testing.T, m *subagentMonitor, cols, rows int) *monitorHarness {
	t.Helper()
	pr, pw := io.Pipe()
	h := &monitorHarness{t: t, emu: term.New(cols, rows, term.Options{}), keys: pw, done: make(chan struct{})}
	h.app = buildMonitorApplication(m, pr, monitorEmulatorWriter{h})
	h.app.cols, h.app.rows = cols, rows
	t.Cleanup(func() {
		h.app.exit()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
		_ = pw.Close()
	})
	return h
}

// Runs the monitor on its own goroutine and waits for its first frame
func (h *monitorHarness) start(firstFrame string) {
	h.t.Helper()
	go func() {
		defer close(h.done)
		h.app.run()
	}()
	h.waitShows(firstFrame)
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

func TestMonitorKeysPinControlsAcrossRosterChanges(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cleanSubagentRegistry(t)
	child := &pinnedChild{}
	for _, sid := range []string{"first", "second"} {
		agent.RegisterSubagent(map[string]any{"subagent_id": sid, "goal": sid, "owner_agent_session_id": "owner",
			"started_at": 1.0, "status": "running", "agent": child})
	}
	dock := newSubagentMonitor(dockCLI(t, ownerAgent(t, "owner"), ""))
	dock.refresh()
	h := newMonitorHarness(t, dock, 32, 14)
	footer := h.app.footer(h.app.cols, h.app.confirm, h.app.steering, dock.detail, false)
	if !strings.Contains(footer, "Ctrl+T") || utf8.RuneCountInString(footer) > 32 {
		t.Fatalf("footer %q", footer)
	}
	pending := dock.cli.t("cli.subagents.control_pending", nil)
	h.start("first")
	h.typeBytes("\x1b[B\rsFocus on tests\r")
	waitFor(t, "steer delivered", func() bool { return len(child.got()) == 1 && h.notice() != pending })
	steered := h.notice()
	h.waitShows("x stop")
	h.typeBytes("x")
	h.waitShows("Stop?")
	agent.UnregisterSubagent("second", nil)
	dock.refresh()
	h.typeBytes("y")
	waitFor(t, "stop answered", func() bool { n := h.notice(); return n != pending && n != steered })
	h.typeBytes("\x1b")
	h.waitShows("Enter tail")
	h.typeBytes("\x14")
	if !h.exited(3 * time.Second) {
		t.Fatalf("monitor still open, screen:\n%s", strings.Join(h.screen(), "\n"))
	}
	if dock.selectedID != "first" {
		t.Fatalf("selected %v", dock.selectedID)
	}
	if got := child.got(); len(got) != 1 || got[0] != "Focus on tests" {
		t.Fatalf("received %q", got)
	}
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

func TestAStalledSteerNeverFreezesTheMonitor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cleanSubagentRegistry(t)
	agent.RegisterSubagent(map[string]any{"subagent_id": "only", "goal": "only", "owner_agent_session_id": "owner",
		"started_at": 1.0, "status": "running", "agent": &pinnedChild{}})
	dock := newSubagentMonitor(dockCLI(t, ownerAgent(t, "owner"), ""))
	dock.refresh()
	release, entered := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	previous := monitorControl
	monitorControl = func(_ *subagentMonitor, action string, message any, target any) (map[string]any, error) {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return map[string]any{"note": "steered late"}, nil
	}
	t.Cleanup(func() { monitorControl = previous })
	h := newMonitorHarness(t, dock, 80, 40)
	h.start("only")
	h.typeBytes("sFocus\r")
	h.waitShows(dock.cli.t("cli.subagents.control_pending", nil))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the steer never reached the backend")
	}
	select {
	case <-release:
		t.Fatal("released before the monitor closed")
	default:
	}
	h.typeBytes("\x1b")
	if !h.exited(3 * time.Second) {
		t.Fatalf("Esc did not close the monitor mid call, screen:\n%s", strings.Join(h.screen(), "\n"))
	}
	close(release)
}
