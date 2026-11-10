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

// Builds an application on a cols by rows emulator, configure runs before the program starts
func newPTHarness(t *testing.T, cols, rows int, configure func(a *ptApplication)) *ptHarness {
	t.Helper()
	pr, pw := io.Pipe()
	h := &ptHarness{t: t, emu: term.New(cols, rows, term.Options{}), keys: pw, done: make(chan error, 1)}
	h.app = newPTApplication(pr, emulatorWriter{h})
	h.app.initialCols, h.app.initialRows = cols, rows
	if configure != nil {
		configure(h.app)
	}
	t.Cleanup(h.stop)
	return h
}

// Starts the program on its own goroutine and waits until it runs
func (h *ptHarness) start() {
	h.t.Helper()
	go func() { h.done <- h.app.run() }()
	waitFor(h.t, "application start", h.app.IsRunning)
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

// A status row, rules and the input buffer, Enter submitting to the channel and Ctrl-D exiting
func basicPTLayout(a *ptApplication, submitted chan string) {
	a.prompt = func() []styledFragment { return []styledFragment{{"class:prompt", "> "}} }
	a.inputHeight = func() int { return max(1, min(8, a.buffer.document().lineCount())) }
	a.layout = []*ptWindow{
		{name: "status", fragments: func() []styledFragment { return []styledFragment{{"class:status-bar", "STATUS ROW"}} }, height: func() int { return 1 }},
		{name: "rule-top", char: "─", height: func() int { return 1 }},
		{name: "input", kind: ptWindowBuffer},
		{name: "rule-bottom", char: "─", height: func() int { return 1 }},
		{name: "menu", kind: ptWindowMenu, maxHeight: 12},
	}
	a.keyBindings.add(func(e *ptEvent) {
		submitted <- e.currentBuffer().text()
		e.currentBuffer().reset(true)
	}, ptBind{}, "enter")
	a.keyBindings.add(func(e *ptEvent) { e.app.Exit() }, ptBind{}, "c-d")
}

func TestPTApplicationTypesSubmitsPrintsAboveAndRestoresTheTerminal(t *testing.T) {
	submitted := make(chan string, 4)
	h := newPTHarness(t, 60, 12, func(a *ptApplication) { basicPTLayout(a, submitted) })
	h.start()
	h.waitShows("STATUS ROW")
	h.waitShows(">")
	if !h.emu.BracketedPaste() || !h.emu.FocusEvents() {
		t.Fatalf("bracketed paste %v focus events %v while running", h.emu.BracketedPaste(), h.emu.FocusEvents())
	}
	h.typeBytes("hello world")
	h.waitShows("> hello world")
	h.typeBytes("\r")
	select {
	case got := <-submitted:
		if got != "hello world" {
			t.Fatalf("submitted %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Enter never submitted")
	}
	h.onLoop(func() { h.app.PrintAbove("the answer is 42") })
	h.waitShows("the answer is 42")
	screen := h.screen()
	answer, status := -1, -1
	for y, row := range screen {
		if strings.Contains(row, "the answer is 42") {
			answer = y
		}
		if strings.Contains(row, "STATUS ROW") {
			status = y
		}
	}
	if answer < 0 || status != answer+1 {
		t.Fatalf("answer row %d status row %d, screen:\n%s", answer, status, strings.Join(screen, "\n"))
	}
	h.typeBytes("\x04")
	if err := h.wait(); err != nil {
		t.Fatalf("run returned %v", err)
	}
	if h.shows("STATUS ROW") || h.shows("─") {
		t.Fatalf("chrome left behind:\n%s", strings.Join(h.screen(), "\n"))
	}
	if !h.shows("the answer is 42") {
		t.Fatalf("scrollback text lost:\n%s", strings.Join(h.screen(), "\n"))
	}
	if h.emu.BracketedPaste() || h.emu.FocusEvents() || h.emu.KittyFlags() != 0 {
		t.Fatalf("modes left on: paste %v focus %v kitty %d", h.emu.BracketedPaste(), h.emu.FocusEvents(), h.emu.KittyFlags())
	}
}

func TestPasteWithoutEndMarkIsDeliveredOnceMoreInputFollowsTheTimeout(t *testing.T) {
	applyBracketedPasteTimeoutPatch()
	fakeMonotonic(t, 100, 100, 103, 103, 103)
	pasted := make(chan string, 1)
	h := newPTHarness(t, 60, 8, func(a *ptApplication) {
		a.layout = []*ptWindow{{name: "input", kind: ptWindowBuffer}}
		a.keyBindings.add(func(e *ptEvent) { pasted <- e.data() }, ptBind{eager: true}, ptKeyBracketedPaste)
	})
	h.start()
	h.typeBytes("\x1b[200~stuck paste")
	time.Sleep(150 * time.Millisecond)
	h.typeBytes(" and more")
	time.Sleep(150 * time.Millisecond)
	h.typeBytes("!")
	select {
	case got := <-pasted:
		if got != "stuck paste and more!" {
			t.Fatalf("pasted %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the paste stayed stuck")
	}
	h.typeBytes("x")
	waitFor(t, "typing after recovery", func() bool { return h.app.BufferText() == "x" })
}

func TestPasteGuardLeavesCompleteAndTornPastesAlone(t *testing.T) {
	fakeMonotonic(t, 10, 10.1, 10.2, 10.3)
	var g ptPasteGuard
	for _, chunk := range []string{"\x1b[200~some content\x1b[20", "1~", "plain", "\x1b[200~whole\x1b[201~"} {
		if got := string(g.filter([]byte(chunk))); got != chunk {
			t.Fatalf("chunk %q became %q", chunk, got)
		}
	}
	if g.inPaste {
		t.Fatal("guard still inside a paste")
	}
}
