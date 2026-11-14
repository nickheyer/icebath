package app

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeTUIApp is the prompt application double the modal and render tests drive
type fakeTUIApp struct {
	mu          sync.Mutex
	running     bool
	loop        bool
	onLoop      bool
	busy        bool
	columns     int
	rows        int
	chromeRows  int
	queued      []func()
	raw         []string
	above       []string
	printedRaw  []string
	inTerminal  []func()
	text        string
	cursor      int
	invalidated int
	resets      int
	exits       int
}

func (a *fakeTUIApp) Exit() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.exits++
	a.running = false
}

func (a *fakeTUIApp) Invalidate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidated++
	return nil
}

func (a *fakeTUIApp) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *fakeTUIApp) OnLoop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.onLoop
}

func (a *fakeTUIApp) CallSoonThreadsafe(fn func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loop {
		return false
	}
	a.queued = append(a.queued, fn)
	return true
}

func (a *fakeTUIApp) WriteRaw(seq string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.raw = append(a.raw, seq, "<flush>")
	return nil
}

func (a *fakeTUIApp) PrintAbove(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.above = append(a.above, text)
}

func (a *fakeTUIApp) PrintRaw(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.printedRaw = append(a.printedRaw, text)
}

func (a *fakeTUIApp) TerminalBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

func (a *fakeTUIApp) RunInTerminal(fn func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inTerminal = append(a.inTerminal, fn)
}

func (a *fakeTUIApp) Size() (int, int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.columns == 0 {
		return 0, 0, false
	}
	return a.columns, a.rows, true
}

func (a *fakeTUIApp) ChromeRows() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chromeRows
}

func (a *fakeTUIApp) BufferText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.text
}

func (a *fakeTUIApp) BufferCursor() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cursor
}

func (a *fakeTUIApp) SetBuffer(text string, cursor int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.text, a.cursor = text, cursor
}

func (a *fakeTUIApp) ResetBuffer() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.text, a.cursor = "", 0
	a.resets++
}

// Number of invalidate calls so far
func (a *fakeTUIApp) invalidations() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.invalidated
}

// Number of buffer resets so far
func (a *fakeTUIApp) bufferResets() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resets
}

// Queued loop callbacks so far
func (a *fakeTUIApp) loopQueue() []func() {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]func(){}, a.queued...)
}

// Raw writes so far
func (a *fakeTUIApp) rawWrites() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.raw...)
}

// Texts printed above the prompt so far
func (a *fakeTUIApp) printedAbove() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string{}, a.above...)
}

// A CLI with a fake application the modal tests drive, config pinned to the given map
func newModalTestCLI(t *testing.T, cfg map[string]any) (*CLI, *fakeTUIApp) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if cfg == nil {
		cfg = map[string]any{}
	}
	SetCLIConfig(cfg)
	t.Cleanup(func() { SetCLIConfig(nil) })
	app := &fakeTUIApp{}
	return &CLI{ctx: context.Background(), app: app}, app
}

// Polls the condition for up to two seconds
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

// Swaps the modal clock for one returning the given readings in order, the last reading repeating
func fakeMonotonic(t *testing.T, readings ...float64) {
	t.Helper()
	prevClock, prevInterval := monotonic, modalPollInterval
	var mu sync.Mutex
	i := 0
	monotonic = func() float64 {
		mu.Lock()
		defer mu.Unlock()
		v := readings[min(i, len(readings)-1)]
		i++
		return v
	}
	modalPollInterval = time.Millisecond
	t.Cleanup(func() { monotonic, modalPollInterval = prevClock, prevInterval })
}

// Registers the fake as the process wide application for the test
func installTUIApp(t *testing.T, app *fakeTUIApp) {
	t.Helper()
	setTUIApp(app)
	t.Cleanup(func() { setTUIApp(nil) })
}
