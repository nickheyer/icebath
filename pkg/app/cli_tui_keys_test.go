package app

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
)

// An idle prompt application with an event for its buffer
func handlerRig() (*ptApplication, *ptEvent) {
	app := newPTApplication(nil, io.Discard)
	return app, &ptEvent{app: app, keySequence: []ptKeyPress{{Key: "c-m", Data: "\r"}}}
}

func TestDraftIsClearedBeforeARunningTurnIsInterrupted(t *testing.T) {
	for name, handler := range map[string]func(*CLI) ptHandler{
		"_tui_handle_ctrl_c": func(c *CLI) ptHandler { return c.tuiHandleCtrlC },
		"_tui_handle_ctrl_q": func(c *CLI) ptHandler { return c.tuiHandleCtrlQ },
	} {
		var interrupts []*agent.AIAgent
		prev := requestHardInterrupt
		requestHardInterrupt = func(a *agent.AIAgent) { interrupts = append(interrupts, a) }
		c := &CLI{ctx: context.Background(), agentRunning: true, agent: &agent.AIAgent{}}
		app, event := handlerRig()
		_ = app.buffer.setText("half typed follow-up")
		handler(c)(event)
		if app.buffer.text() != "" || len(interrupts) != 0 || c.shouldExit {
			t.Fatalf("%s first press: text %q interrupts %d exit %v", name, app.buffer.text(), len(interrupts), c.shouldExit)
		}
		handler(c)(event)
		if len(interrupts) != 1 || interrupts[0] != c.agent || c.shouldExit {
			t.Fatalf("%s second press: interrupts %v exit %v", name, interrupts, c.shouldExit)
		}
		requestHardInterrupt = prev
	}
}

// burstRig is a prompt on a controlled clock whose timers and loop hops run on demand
type burstRig struct {
	c         *CLI
	app       *ptApplication
	buf       *ptBuffer
	now       float64
	mu        sync.Mutex
	scheduled []func()
	delays    []time.Duration
	busy      []string
}

// Builds the rig with the given agent state, inline commands declined
func newBurstRig(t *testing.T, agentRunning bool) *burstRig {
	t.Helper()
	r := &burstRig{now: 1000}
	prev := monotonic
	monotonic = func() float64 {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.now
	}
	t.Cleanup(func() { monotonic = prev })
	r.c = &CLI{ctx: context.Background(), agentRunning: agentRunning, pendingInput: &inputQueue{}, interruptQueue: &inputQueue{}, config: map[string]any{}}
	r.c.tui.inlineCommandHook = func(*ptEvent, string, bool) bool { return false }
	r.c.tui.enterWhileBusyHook = func(text string, _ []string, _ any, _ string) { r.busy = append(r.busy, text) }
	r.app = newPTApplication(nil, io.Discard)
	r.app.running.Store(true)
	t.Cleanup(func() { r.app.running.Store(false) })
	r.app.callLaterHook = func(d time.Duration, fn func()) {
		r.delays = append(r.delays, d)
		r.scheduled = append(r.scheduled, fn)
	}
	r.app.callSoonHook = func(fn func()) { fn() }
	r.buf = r.app.buffer
	r.buf.onTextChanged = append(r.buf.onTextChanged, r.c.tuiOnTextChanged)
	return r
}

func (r *burstRig) advance(seconds float64) {
	r.mu.Lock()
	r.now += seconds
	r.mu.Unlock()
}

// Presses Enter with or without more keys of the same read still queued
func (r *burstRig) enter(moreKeysInRead bool) {
	r.app.processor.inputQueue = nil
	if moreKeysInRead {
		r.app.processor.inputQueue = []ptKeyPress{{Key: "x", Data: "x"}}
	}
	r.c.tuiHandleEnter(&ptEvent{app: r.app, keySequence: []ptKeyPress{{Key: "c-m", Data: "\r"}}})
}

// Lets the quiet window pass with no input, then fires what was scheduled
func (r *burstRig) runScheduled() {
	pending, delays := r.scheduled, r.delays
	r.scheduled, r.delays = nil, nil
	for i, fn := range pending {
		r.advance(delays[i].Seconds())
		fn()
	}
}

// Everything submitted to the pending input queue so far
func (r *burstRig) submitted() []any {
	var items []any
	for {
		item, ok := r.c.pendingInput.GetNoWait()
		if !ok {
			return items
		}
		items = append(items, item)
	}
}

func (r *burstRig) typeText(text string) { r.buf.insertText(text, false, true, true) }

func TestTextAndEnterInOneWriteSubmitsAfterTheQuietWindow(t *testing.T) {
	r := newBurstRig(t, false)
	r.typeText("/help")
	r.enter(false)
	if got := r.submitted(); len(got) != 0 || r.buf.text() != "/help\n" {
		t.Fatalf("submitted %v buffer %q", got, r.buf.text())
	}
	if len(r.delays) != 1 || r.delays[0] != time.Duration(burstEndQuiet*float64(time.Second)) {
		t.Fatalf("scheduled %v", r.delays)
	}
	r.runScheduled()
	if got := r.submitted(); !reflect.DeepEqual(got, []any{"/help"}) || r.buf.text() != "" {
		t.Fatalf("submitted %v buffer %q", got, r.buf.text())
	}
}

func TestScriptedEditInsideADraftSubmitsTheWholeDraft(t *testing.T) {
	r := newBurstRig(t, false)
	_ = r.buf.setDocument("hello world", 5, true)
	r.advance(10)
	r.typeText(", big")
	r.enter(false)
	if r.buf.text() != "hello, big\n world" {
		t.Fatalf("buffer %q", r.buf.text())
	}
	r.runScheduled()
	if got := r.submitted(); !reflect.DeepEqual(got, []any{"hello, big world"}) {
		t.Fatalf("submitted %v", got)
	}
}

func TestEnterAfterAPauseIsAnOrdinarySubmit(t *testing.T) {
	r := newBurstRig(t, false)
	r.typeText("hi")
	r.advance(rapidInputEnterWindow * 2)
	r.enter(false)
	if got := r.submitted(); !reflect.DeepEqual(got, []any{"hi"}) || len(r.scheduled) != 0 {
		t.Fatalf("submitted %v scheduled %d", got, len(r.scheduled))
	}
}

func TestUnbracketedMultilinePasteKeepsNewlinesAndNeverSubmits(t *testing.T) {
	r := newBurstRig(t, false)
	r.typeText("line one")
	r.enter(true)
	r.typeText("line two")
	r.enter(true)
	r.typeText("line three")
	r.enter(false)
	r.runScheduled()
	if got := r.submitted(); r.buf.text() != "line one\nline two\nline three\n" || len(got) != 0 {
		t.Fatalf("buffer %q submitted %v", r.buf.text(), got)
	}
	r = newBurstRig(t, false)
	r.typeText("chunk one")
	r.enter(false)
	r.advance(burstEndQuiet / 5)
	r.typeText("chunk two")
	r.runScheduled()
	if got := r.submitted(); r.buf.text() != "chunk one\nchunk two" || len(got) != 0 {
		t.Fatalf("buffer %q submitted %v", r.buf.text(), got)
	}
}

func TestPasteWhoseReadsContinuePastTheQuietWindowNeverSubmits(t *testing.T) {
	r := newBurstRig(t, false)
	r.typeText("one")
	r.enter(false)
	for _, chunk := range []string{"two", " continued", " again", " end"} {
		r.advance(0.1)
		r.typeText(chunk)
	}
	r.enter(false)
	r.runScheduled()
	if got := r.submitted(); r.buf.text() != "one\ntwo continued again end\n" || len(got) != 0 {
		t.Fatalf("buffer %q submitted %v", r.buf.text(), got)
	}
}

func TestChunkWaitingThroughAnEventLoopStallIsReadBeforeTheQuietWindowEnds(t *testing.T) {
	r := newBurstRig(t, false)
	waiting := ""
	var hops []func()
	r.app.callSoonHook = func(fn func()) { hops = append(hops, fn) }
	r.app.inputWaitingFn = func() bool { return waiting != "" }
	read := func() {
		text, enter, _ := strings.Cut(waiting, "\r")
		hasEnter := strings.Contains(waiting, "\r")
		waiting = ""
		r.typeText(text)
		if hasEnter {
			_ = enter
			r.enter(false)
		}
	}
	r.typeText("one")
	r.enter(false)
	r.advance(burstEndQuiet + 0.15)
	waiting = "two end\r"
	r.runScheduled()
	for pass := 0; pass < 8 && len(hops) > 0; pass++ {
		pending := hops
		hops = nil
		for _, hop := range pending {
			hop()
		}
		if waiting != "" {
			read()
		}
	}
	r.advance(burstEndQuiet)
	r.runScheduled()
	for len(hops) > 0 {
		pending := hops
		hops = nil
		for _, hop := range pending {
			hop()
		}
	}
	if got := r.submitted(); r.buf.text() != "one\ntwo end\n" || len(got) != 0 {
		t.Fatalf("buffer %q submitted %v", r.buf.text(), got)
	}
}

func TestEnterRightAfterTextArrivesIsANewlineNotASteer(t *testing.T) {
	r := newBurstRig(t, true)
	r.app.running.Store(false)
	r.typeText("line one")
	r.enter(false)
	if r.buf.text() != "line one\n" || len(r.busy) != 0 {
		t.Fatalf("buffer %q busy %v", r.buf.text(), r.busy)
	}
	r.typeText("line two")
	r.advance(rapidInputEnterWindow * 3)
	r.enter(false)
	if !reflect.DeepEqual(r.busy, []string{"line one\nline two"}) {
		t.Fatalf("busy submissions %q", r.busy)
	}
}

func TestFastBackslashContinuationStillConsumesTheBackslash(t *testing.T) {
	r := newBurstRig(t, true)
	r.app.running.Store(false)
	r.c.tui.multilineShortcuts = true
	r.typeText("alpha \\")
	r.enter(false)
	if r.buf.text() != "alpha \n" || len(r.busy) != 0 {
		t.Fatalf("buffer %q busy %v", r.buf.text(), r.busy)
	}
}

func TestKeypadAltEnterInsertsNewlinesAtTheCursor(t *testing.T) {
	for _, prefix := range []string{"alpha", "alpha  ", "alpha!?.,", "[Pasted text #1: 15 lines]"} {
		for _, sequence := range []string{"\x1b[57414;3u", "\x1b[57414;131u", "\x1b[13;3u", "\x1b\r"} {
			result := make(chan string, 1)
			h := newPTHarness(t, 80, 10, func(a *ptApplication) {
				a.layout = []*ptWindow{{name: "input", kind: ptWindowBuffer}}
				a.inputHeight = func() int { return 4 }
				_ = a.buffer.setDocument(prefix+"suffix", len([]rune(prefix)), true)
				a.keyBindings.add((&CLI{}).tuiInsertNewline, ptBind{}, "escape", "enter")
				a.keyBindings.add(func(e *ptEvent) {
					result <- e.currentBuffer().text()
					e.app.Exit()
				}, ptBind{}, "c-q")
			})
			h.start()
			h.typeBytes(sequence)
			waitFor(t, "first newline", func() bool { return strings.Count(h.app.BufferText(), "\n") == 1 })
			h.typeBytes(sequence)
			waitFor(t, "second newline", func() bool { return strings.Count(h.app.BufferText(), "\n") == 2 })
			h.typeBytes("\x11")
			select {
			case got := <-result:
				if got != prefix+"\n\nsuffix" {
					t.Fatalf("prefix %q sequence %q: buffer %q", prefix, sequence, got)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("prefix %q sequence %q: never exited", prefix, sequence)
			}
			h.stop()
		}
	}
}

func TestFooterSplitClipsFromTheTopWhenMinimumsOverflow(t *testing.T) {
	app := newPTApplication(nil, io.Discard)
	sizes := footerSplitDivideHeights(app, []ptDim{{1, 1, 1}, {1, 1, 1}, {1, 1, 1}}, 2)
	if !reflect.DeepEqual(sizes, []int{0, 1, 1}) {
		t.Fatalf("sizes %v, the late top row must be the one clipped", sizes)
	}
	if app.invalidations.Load() == 0 {
		t.Fatal("no repaint was queued after clipping")
	}
}

func TestPromptHandlersSubmitALineAndExitOnCtrlD(t *testing.T) {
	c := &CLI{ctx: context.Background(), pendingInput: &inputQueue{}, interruptQueue: &inputQueue{}, config: map[string]any{}}
	c.tui.inlineCommandHook = func(*ptEvent, string, bool) bool { return false }
	h := newPTHarness(t, 60, 10, func(a *ptApplication) {
		a.prompt = func() []styledFragment { return []styledFragment{{"class:prompt", "❯ "}} }
		a.inputHeight = func() int { return 1 }
		a.layout = []*ptWindow{{name: "rule", char: "─", height: oneRow}, {name: "input", kind: ptWindowBuffer}}
		a.buffer.onTextChanged = append(a.buffer.onTextChanged, c.tuiOnTextChanged)
		a.keyBindings.add(c.tuiHandleEnter, ptBind{}, "enter")
		a.keyBindings.add(c.tuiHandleCtrlD, ptBind{}, "c-d")
	})
	c.app = h.app
	h.start()
	h.waitShows("❯")
	h.typeBytes("what is the answer")
	h.waitShows("what is the answer")
	time.Sleep(time.Duration(rapidInputEnterWindow*3*float64(time.Second)) + 20*time.Millisecond)
	h.typeBytes("\r")
	item, ok := c.pendingInput.Get(3 * time.Second)
	if !ok || item != "what is the answer" {
		t.Fatalf("pending input %v %v", item, ok)
	}
	waitFor(t, "buffer reset", func() bool { return h.app.BufferText() == "" })
	h.typeBytes("\x04")
	if err := h.wait(); err != nil {
		t.Fatalf("run returned %v", err)
	}
	if !c.exitRequested() || h.shows("─") {
		t.Fatalf("exit flag %v, screen:\n%s", c.exitRequested(), strings.Join(h.screen(), "\n"))
	}
	if h.emu.BracketedPaste() || h.emu.FocusEvents() || h.emu.KittyFlags() != 0 {
		t.Fatalf("modes left on: paste %v focus %v kitty %d", h.emu.BracketedPaste(), h.emu.FocusEvents(), h.emu.KittyFlags())
	}
}
