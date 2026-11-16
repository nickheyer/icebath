package cliui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/term"
)

// Console over scripted input drawing on the project terminal emulator
type rig struct {
	c    *Console
	emu  *term.Emulator
	mu   sync.Mutex
	rows int
	cols int
	env  map[string]string
}

// Writer turning a bare line feed into a carriage return and line feed, as a cooked terminal does
type cookedOut struct {
	w    io.Writer
	last byte
}

func (o *cookedOut) Write(p []byte) (int, error) {
	var b bytes.Buffer
	for _, c := range p {
		if c == '\n' && o.last != '\r' {
			b.WriteByte('\r')
		}
		b.WriteByte(c)
		o.last = c
	}
	_, err := o.w.Write(b.Bytes())
	return len(p), err
}

func newRig(input io.Reader, cols, rows int) *rig {
	r := &rig{emu: term.New(cols, rows, term.Options{Scrollback: 500}), rows: rows, cols: cols,
		env: map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}}
	r.c = &Console{In: input, Out: &cookedOut{w: r.emu},
		Getenv:    func(k string) (string, bool) { v, ok := r.env[k]; return v, ok },
		StdinTTY:  func() bool { return true },
		StdoutTTY: func() bool { return true },
		TermSize: func() (int, int, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.rows, r.cols, nil
		}}
	return r
}

func keysRig(input string) *rig { return newRig(strings.NewReader(input), 100, 30) }

// Rows the emulator shows, scrollback first, each without trailing blanks
func (r *rig) text() string {
	var b strings.Builder
	base := r.emu.Base()
	_, h := r.emu.Size()
	first := base - uint64(r.emu.ScrollbackLen())
	for abs := first; abs < base+uint64(h); abs++ {
		line, ok := r.emu.Row(abs)
		if !ok {
			continue
		}
		var row strings.Builder
		for _, cell := range line.Cells {
			switch {
			case cell.Content != "":
				row.WriteString(cell.Content)
			case cell.Width != 0:
				row.WriteByte(' ')
			}
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r *rig) resize(cols, rows int) {
	r.mu.Lock()
	r.rows, r.cols = rows, cols
	r.mu.Unlock()
	r.emu.Resize(cols, rows)
}

func items(labels ...string) []RadioItem {
	out := make([]RadioItem, len(labels))
	for i, l := range labels {
		out[i] = PlainItem(l)
	}
	return out
}

func radio(t *testing.T, r *rig, selected int, opts RadiolistOptions, labels ...string) int {
	t.Helper()
	got, err := r.c.CursesRadiolist(context.Background(), "Pick one:", items(labels...), selected, opts)
	if err != nil {
		t.Fatalf("radiolist: %v\n%s", err, r.text())
	}
	return got
}

func cancelAt(n int) RadiolistOptions { return RadiolistOptions{CancelReturns: &n} }

func TestArrowAndViKeysMoveTheCursorAndEnterSelects(t *testing.T) {
	for keys, want := range map[string]int{"\r": 0, "\x1b[B\r": 1, "\x1b[B\x1b[B\r": 2, "\x1b[A\r": 2, "jj\r": 2, "jk\r": 0,
		"\x1bOB\r": 1, "\x1bOA\r": 2, "\x1b[B ": 1, "\x1b[B\n": 1, "\x1b[6~\r": 2, "\x1b[F\x1b[H\r": 0} {
		r := keysRig(keys)
		if got := radio(t, r, 0, cancelAt(-1), "a", "b", "c"); got != want {
			t.Fatalf("keys %q picked %d, want %d", keys, got, want)
		}
		name := []string{"a", "b", "c"}[want]
		if !strings.Contains(r.text(), "› Pick one  "+name) {
			t.Fatalf("keys %q left no answer line for %s:\n%s", keys, name, r.text())
		}
	}
}

func TestCancelKeysReturnTheCancelValue(t *testing.T) {
	for _, keys := range []string{"\x1b", "q", "\x03", "\x04", "\x1b[27u", "\x1b[99;5u", ""} {
		r := keysRig(keys)
		got, err := r.c.CursesRadiolist(context.Background(), "Pick one:", items("a", "b"), 1, cancelAt(-7))
		if keys == "" {
			if !errors.Is(err, ErrEOF) {
				t.Fatalf("closed input gave %d, %v", got, err)
			}
			continue
		}
		if err != nil || got != -7 {
			t.Fatalf("keys %q gave %d, %v", keys, got, err)
		}
		if !strings.Contains(r.text(), "› Pick one  cancelled") {
			t.Fatalf("keys %q left no cancelled line:\n%s", keys, r.text())
		}
	}
}

func TestEnhancedKeyboardEnterSelects(t *testing.T) {
	r := keysRig("\x1b[B\x1b[13u")
	if got := radio(t, r, 0, cancelAt(-1), "a", "b"); got != 1 {
		t.Fatalf("picked %d", got)
	}
}

func TestCancelDefaultsToTheSelection(t *testing.T) {
	r := keysRig("\x1b[B\x1b")
	if got := radio(t, r, 2, RadiolistOptions{}, "a", "b", "c"); got != 2 {
		t.Fatalf("cancel returned %d, want the selection 2", got)
	}
}

func TestNonTTYStdinReturnsCancelValue(t *testing.T) {
	r := keysRig("\r")
	r.c.StdinTTY = func() bool { return false }
	if got := radio(t, r, 0, cancelAt(-3), "a", "b"); got != -3 {
		t.Fatalf("got %d", got)
	}
	if r.text() != "" {
		t.Fatalf("drew without a terminal:\n%s", r.text())
	}
}

func TestSearchFiltersAndSelectsTheBestMatch(t *testing.T) {
	r := keysRig("/son\r")
	got := radio(t, r, 0, RadiolistOptions{CancelReturns: new(int), Searchable: true}, "gpt-4o", "claude-opus", "claude-sonnet", "gemini")
	if got != 2 {
		t.Fatalf("picked %d", got)
	}
}

func TestSearchUsesTheGivenLabelsAndEditingKeys(t *testing.T) {
	opts := RadiolistOptions{CancelReturns: new(int), Searchable: true, SearchLabels: []string{"alpha one", "beta two", "gamma three"}}
	r := keysRig("/twox\x7f\r")
	if got := radio(t, r, 0, opts, "a", "b", "c"); got != 1 {
		t.Fatalf("backspace edit picked %d", got)
	}
	r = keysRig("/zzz\x15three\r")
	if got := radio(t, r, 0, opts, "a", "b", "c"); got != 2 {
		t.Fatalf("ctrl u edit picked %d", got)
	}
	r = keysRig("/zzz\r\x15\x1b[B\r")
	if got := radio(t, r, 0, opts, "a", "b", "c"); got != 1 {
		t.Fatalf("enter with no match then a cleared query and down picked %d", got)
	}
}

func TestSearchKeepsViKeysAsQueryText(t *testing.T) {
	r := keysRig("/jk\x1b[B\r")
	got := radio(t, r, 0, RadiolistOptions{CancelReturns: new(int), Searchable: true}, "jkl", "xjkx", "none")
	if got != 1 {
		t.Fatalf("picked %d", got)
	}
}

func TestHeadingsAreDrawnButNeverSelected(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan int, 1)
	go func() {
		got, _ := r.c.CursesRadiolist(context.Background(), "Models", items("a", "b", "c"), 0,
			RadiolistOptions{CancelReturns: new(int), Headings: map[int]string{0: "Fast", 2: "Large"}})
		done <- got
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "Fast") && strings.Contains(s, "Large") })
	io.WriteString(pipeOut, "\x1b[B\x1b[B\r")
	if got := <-done; got != 2 {
		t.Fatalf("picked %d", got)
	}
}

// Waits until the screen satisfies cond
func waitFor(t *testing.T, r *rig, cond func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(r.text()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("screen never matched:\n%s", r.text())
}

func TestLabelsSplitIntoNameExplanationAndStateMark(t *testing.T) {
	for label, want := range map[string][3]string{
		"Anthropic (Claude models via API key)":                     {"Anthropic", "Claude models via API key", ""},
		"Quick Setup (Nous Portal) — free login (recommended)":      {"Quick Setup (Nous Portal)", "free login (recommended)", ""},
		"Local - run directly on this machine (default)":            {"Local", "run directly on this machine (default)", ""},
		"claude-fable-5.1  ← currently in use":                      {"claude-fable-5.1", "", "currently in use"},
		"Anthropic (Claude models via API key)  ← currently active": {"Anthropic", "Claude models via API key", "currently active"},
		"Hugging Face Inference Providers":                          {"Hugging Face Inference Providers", "", ""},
		"a (b) (c)":                                                 {"a (b)", "c", ""},
		"🎬 Video Analysis (video_analyze (requires a video model))": {"🎬 Video Analysis", "video_analyze (requires a video model)", ""},
		"Local models (run open models here — no account)":          {"Local models", "run open models here — no account", ""},
	} {
		name, explanation, tag := splitLabel(label)
		if got := [3]string{name, explanation, tag}; got != want {
			t.Fatalf("%q split into %q", label, got)
		}
	}
}

func TestRowsShowNamesInOneColumnWithExplanationsBeside(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Backend", items("Local - on this machine", "Docker/Podman - in a container"), 0, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "❯ Local          on this machine") && strings.Contains(s, "  Docker/Podman  in a container")
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if !strings.Contains(r.text(), "› Backend  Local") {
		t.Fatalf("answer line names more than the choice:\n%s", r.text())
	}
}

func TestLongListScrollsWithCountsOfHiddenRows(t *testing.T) {
	labels := make([]string, 30)
	for i := range labels {
		labels[i] = "row" + string(rune('A'+i%26)) + string(rune('0'+i/26))
	}
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan int, 1)
	go func() {
		got, _ := r.c.CursesRadiolist(context.Background(), "Many", items(labels...), 0, cancelAt(-1))
		done <- got
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "↓ 18 more") && strings.Count(s, " more") == 1 })
	io.WriteString(pipeOut, strings.Repeat("\x1b[B", 14))
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "↑ 3 more") && strings.Contains(s, "↓ 15 more") })
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != 14 {
		t.Fatalf("picked %d", got)
	}
}

func TestLongDescriptionKeepsChoicesAndHintOnAShortTerminal(t *testing.T) {
	var desc []string
	for i := 0; i < 40; i++ {
		desc = append(desc, "description line")
	}
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 14)
	done := make(chan int, 1)
	go func() {
		got, _ := r.c.CursesRadiolist(context.Background(), "Pick", items("a", "b", "c", "d", "e", "f", "g"), 6,
			RadiolistOptions{CancelReturns: new(int), Description: strings.Join(desc, "\n")})
		done <- got
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "more description lines (enlarge terminal)") && strings.Contains(s, "❯ g") && strings.Contains(s, "enter select")
	})
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != 6 {
		t.Fatalf("picked %d", got)
	}
}

func TestFrameNeverExceedsTheTerminalAndLeavesNoRowsBehind(t *testing.T) {
	labels := make([]string, 40)
	for i := range labels {
		labels[i] = "choice"
	}
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 60, 8)
	io.WriteString(r.c.Out, "earlier output\n")
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Tight", items(labels...), 0, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "● Tight") })
	io.WriteString(pipeOut, strings.Repeat("\x1b[B", 30)+"\r")
	<-done
	if got := r.text(); got != "earlier output\n  › Tight  choice" {
		t.Fatalf("screen after the prompt:\n%q", got)
	}
}

func TestResizeRepaintsFromTheRegionTopWithoutLeftovers(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	io.WriteString(r.c.Out, "before\n")
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Resizable", items("Alpha - the first long explanation of a choice", "Beta - the second long explanation"), 0, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "the first long explanation of a choice") })
	r.resize(34, 30)
	waitFor(t, r, func(s string) bool {
		return strings.Count(s, "● Resizable") == 1 && strings.Contains(s, strings.Join([]string{
			"    ❯ Alpha  the first long",
			"             explanation of a",
			"             choice",
			"      Beta   the second long",
			"             explanation",
			"",
			"    ↑↓ move  ·  enter select",
			"    esc cancel",
		}, "\n"))
	})
	r.resize(100, 30)
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "the first long explanation of a choice") && strings.Count(s, "● Resizable") == 1
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if got := r.text(); got != "before\n  › Resizable  Alpha" {
		t.Fatalf("screen after resizes:\n%q", got)
	}
}

func TestChecklistTogglesConfirmsAndCancels(t *testing.T) {
	r := keysRig(" \x1b[B\x1b[B \r")
	got, err := r.c.CursesChecklist(context.Background(), "Tools", []string{"web", "image", "tts"}, map[int]bool{1: true}, nil,
		func(set map[int]bool) string { return "picked " + string(rune('0'+len(set))) })
	if err != nil || !reflect.DeepEqual(got, map[int]bool{0: true, 1: true, 2: true}) {
		t.Fatalf("got %v %v", got, err)
	}
	if !strings.Contains(r.text(), "› Tools  web, image, tts") {
		t.Fatalf("answer line:\n%s", r.text())
	}
	r = keysRig(" \x1b")
	got, err = r.c.CursesChecklist(context.Background(), "Tools", []string{"web", "image"}, map[int]bool{1: true}, nil, nil)
	if err != nil || !reflect.DeepEqual(got, map[int]bool{1: true}) {
		t.Fatalf("cancel got %v %v", got, err)
	}
	r = keysRig(" \x1b")
	got, _ = r.c.CursesChecklist(context.Background(), "Tools", []string{"web"}, nil, map[int]bool{9: true}, nil)
	if !reflect.DeepEqual(got, map[int]bool{9: true}) {
		t.Fatalf("cancel with explicit value got %v", got)
	}
}

func TestChecklistShowsItsStatusLine(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan struct{})
	go func() {
		r.c.CursesChecklist(context.Background(), "Tools", []string{"web", "image"}, nil, nil,
			func(set map[int]bool) string { return "est. " + string(rune('0'+len(set))) + " tokens" })
		close(done)
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "est. 0 tokens") && strings.Contains(s, "[ ] web") })
	io.WriteString(pipeOut, " ")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "est. 1 tokens") && strings.Contains(s, "[✓] web") })
	io.WriteString(pipeOut, "\r")
	<-done
}

func TestSingleSelectPicksAnItemOrTheCancelRow(t *testing.T) {
	r := keysRig("\x1b[B\r")
	idx, ok, err := r.c.CursesSingleSelect(context.Background(), "One", []string{"a", "b"}, 0, "Cancel", false)
	if err != nil || !ok || idx != 1 {
		t.Fatalf("got %d %v %v", idx, ok, err)
	}
	r = keysRig("\x1b[B\x1b[B\r")
	if _, ok, _ := r.c.CursesSingleSelect(context.Background(), "One", []string{"a", "b"}, 0, "Cancel", false); ok {
		t.Fatal("the cancel row counted as a pick")
	}
	r = keysRig("q")
	if _, ok, _ := r.c.CursesSingleSelect(context.Background(), "One", []string{"a", "b"}, 0, "Cancel", false); ok {
		t.Fatal("q counted as a pick")
	}
	r = keysRig("/b\r")
	if idx, ok, _ := r.c.CursesSingleSelect(context.Background(), "One", []string{"apple", "berry"}, 0, "Cancel", true); !ok || idx != 1 {
		t.Fatalf("search picked %d %v", idx, ok)
	}
}

func TestYesNoListTakesTheLetterKeys(t *testing.T) {
	r := keysRig("n")
	if got := radioOf(t, r, "Proceed?", 0, "Yes", "No"); got != 1 {
		t.Fatalf("n picked %d", got)
	}
	r = keysRig("y")
	if got := radioOf(t, r, "Proceed?", 1, "Yes", "No"); got != 0 {
		t.Fatalf("y picked %d", got)
	}
	r = keysRig("y\r")
	if got := radioOf(t, r, "Other?", 1, "Maybe", "No"); got != 1 {
		t.Fatalf("y on a list without yes and no picked %d", got)
	}
}

func radioOf(t *testing.T, r *rig, title string, selected int, labels ...string) int {
	t.Helper()
	got, err := r.c.CursesRadiolist(context.Background(), title, items(labels...), selected, cancelAt(-1))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Navigation handler recording its events, answering begin with the given start
type navLog struct {
	events []string
	values []any
	start  MenuNavigationStart
}

func (n *navLog) handle(event MenuNavigationEvent, value ...any) (*MenuNavigationStart, error) {
	n.events = append(n.events, string(event))
	if len(value) > 0 {
		n.values = append(n.values, value[0])
	}
	if event == NavEventBegin {
		s := n.start
		return &s, nil
	}
	return nil, nil
}

func scoped(n *navLog) context.Context {
	return WithMenuNavigationHandler(context.Background(), n.handle)
}

func TestScopedListSendsBeginWithItsTitleThenResolve(t *testing.T) {
	n := &navLog{}
	r := keysRig("\x1b[B\r")
	got, err := r.c.CursesRadiolist(scoped(n), "  Pick one:", items("a", "b"), 0, cancelAt(-1))
	if err != nil || got != 1 {
		t.Fatalf("got %d %v", got, err)
	}
	if !reflect.DeepEqual(n.events, []string{"begin", "resolve"}) || !reflect.DeepEqual(n.values, []any{"Pick one", 1}) {
		t.Fatalf("events %v values %v", n.events, n.values)
	}
}

func TestScopedLeftArrowGoesBackOnlyWhenAllowed(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{AllowBack: true}}
	r := keysRig("\x1b[D")
	got, err := r.c.CursesRadiolist(scoped(n), "Pick", items("a", "b"), 0, cancelAt(-1))
	if err != nil || got != -1 || !reflect.DeepEqual(n.events, []string{"begin", "back", "resolve"}) {
		t.Fatalf("got %d %v events %v", got, err, n.events)
	}
	if strings.Contains(r.text(), "Pick") {
		t.Fatalf("going back left a line behind:\n%s", r.text())
	}
	n = &navLog{}
	r = keysRig("\x1b[D\r")
	got, _ = r.c.CursesRadiolist(scoped(n), "Pick", items("a", "b"), 0, cancelAt(-1))
	if got != 0 || !reflect.DeepEqual(n.events, []string{"begin", "resolve"}) {
		t.Fatalf("left without back allowed gave %d events %v", got, n.events)
	}
}

func TestScopedEscapeCancelsOnlyOnTheSecondPress(t *testing.T) {
	single := &navLog{}
	if got, _ := keysRig("\x1b").c.CursesRadiolist(scoped(single), "Pick", items("a", "b"), 0, cancelAt(-1)); got != -1 ||
		!reflect.DeepEqual(single.events, []string{"begin", "cancel", "resolve"}) {
		t.Fatalf("one escape without the guard gave %d events %v", got, single.events)
	}
	pipeIn, pipeOut := io.Pipe()
	n := &navLog{start: MenuNavigationStart{GuardCancel: true}}
	r := newRig(pipeIn, 100, 30)
	done := make(chan int, 1)
	go func() {
		got, _ := r.c.CursesRadiolist(scoped(n), "Pick", items("a", "b"), 0, cancelAt(-1))
		done <- got
	}()
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "esc cancel") })
	io.WriteString(pipeOut, "\x1b")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "press esc again to cancel") })
	io.WriteString(pipeOut, "\x1b[B")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "❯ b") && strings.Contains(s, "esc cancel") })
	io.WriteString(pipeOut, "\x1b")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "press esc again to cancel") })
	io.WriteString(pipeOut, "\x1b")
	if got := <-done; got != -1 || !reflect.DeepEqual(n.events, []string{"begin", "cancel", "resolve"}) {
		t.Fatalf("got %d events %v", got, n.events)
	}
}

func TestReplayedAnswerResolvesWithoutAsking(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{Replay: true, ReplayValue: 1}}
	r := keysRig("")
	got, err := r.c.CursesRadiolist(scoped(n), "Pick", items("a", "b"), 0, cancelAt(-1))
	if err != nil || got != 1 || !reflect.DeepEqual(n.events, []string{"begin", "resolve"}) {
		t.Fatalf("got %d %v events %v", got, err, n.events)
	}
	if r.text() != "  › Pick  b" {
		t.Fatalf("replay drew:\n%q", r.text())
	}
}

func TestReplayValueOfTheWrongTypeIsAnError(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{Replay: true, ReplayValue: "text"}}
	if _, err := keysRig("").c.CursesRadiolist(scoped(n), "Pick", items("a"), 0, cancelAt(-1)); err == nil {
		t.Fatal("a text replay value passed as a row index")
	}
}

func TestNavigationHandlerErrorAbortsTheList(t *testing.T) {
	boom := errors.New("handler broke")
	ctx := WithMenuNavigationHandler(context.Background(), func(MenuNavigationEvent, ...any) (*MenuNavigationStart, error) { return nil, boom })
	if _, err := keysRig("\r").c.CursesRadiolist(ctx, "Pick", items("a"), 0, cancelAt(-1)); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestHandlerPanicOnBackLeavesTheCursorShownAndNoHold(t *testing.T) {
	r := keysRig("\x1b[D")
	ctx := WithMenuNavigationHandler(context.Background(), func(event MenuNavigationEvent, _ ...any) (*MenuNavigationStart, error) {
		if event == NavEventBack {
			panic("back")
		}
		return &MenuNavigationStart{AllowBack: true}, nil
	})
	func() {
		defer func() {
			if recover() != "back" {
				t.Fatal("the back panic did not unwind")
			}
		}()
		r.c.CursesRadiolist(ctx, "Pick", items("a", "b"), 0, cancelAt(-1))
	}()
	g := r.c.termGuard()
	if g.holds != 0 || len(g.modes) != 0 {
		t.Fatalf("holds %d modes %v after the panic", g.holds, g.modes)
	}
	if !r.emu.Cursor().Visible {
		t.Fatal("cursor still hidden after the panic")
	}
}

func dumb(input string) *rig {
	r := keysRig(input)
	r.env["TERM"] = "dumb"
	return r
}

func TestDumbTerminalUsesTheNumberedLists(t *testing.T) {
	r := dumb("2\n")
	if got := radio(t, r, 0, cancelAt(-1), "a", "b"); got != 1 {
		t.Fatalf("radio picked %d", got)
	}
	if !strings.Contains(r.text(), "Select by number, Enter to confirm.") || !strings.Contains(r.text(), "(●)  1. a") {
		t.Fatalf("numbered radio:\n%s", r.text())
	}
	if got := radio(t, dumb("\n"), 1, cancelAt(-1), "a", "b"); got != 1 {
		t.Fatalf("blank line picked %d", got)
	}
	if got := radio(t, dumb("x\n"), 1, cancelAt(-9), "a", "b"); got != -9 {
		t.Fatalf("junk picked %d", got)
	}
	if got := radio(t, dumb(""), 1, cancelAt(-9), "a", "b"); got != -9 {
		t.Fatalf("closed input picked %d", got)
	}
	set, err := dumb("1\n3\n1\n\n").c.CursesChecklist(context.Background(), "Tools", []string{"a", "b", "c"}, nil, nil, nil)
	if err != nil || !reflect.DeepEqual(set, map[int]bool{2: true}) {
		t.Fatalf("checklist got %v %v", set, err)
	}
	idx, ok, _ := dumb("2\n").c.CursesSingleSelect(context.Background(), "One", []string{"a", "b"}, 0, "Cancel", false)
	if !ok || idx != 1 {
		t.Fatalf("single got %d %v", idx, ok)
	}
	if _, ok, _ := dumb("3\n").c.CursesSingleSelect(context.Background(), "One", []string{"a", "b"}, 0, "Cancel", false); ok {
		t.Fatal("the numbered cancel row counted as a pick")
	}
}

func TestNumberedListSendsScopedCancelOnClosedInput(t *testing.T) {
	n := &navLog{}
	got, err := dumb("").c.CursesRadiolist(scoped(n), "Pick", items("a", "b"), 0, cancelAt(-1))
	if err != nil || got != -1 || !reflect.DeepEqual(n.events, []string{"begin", "cancel", "resolve"}) {
		t.Fatalf("got %d %v events %v", got, err, n.events)
	}
}

func TestNumberedHeadingsArePrintedAndNeverNumbered(t *testing.T) {
	r := dumb("2\n")
	got, _ := r.c.CursesRadiolist(context.Background(), "Models", items("a", "b"), 0,
		RadiolistOptions{CancelReturns: new(int), Headings: map[int]string{1: "Large"}})
	if got != 1 || !strings.Contains(r.text(), "  Large\n  (○)  2. b") {
		t.Fatalf("picked %d:\n%s", got, r.text())
	}
}

func lineOf(t *testing.T, input string) (string, *rig) {
	t.Helper()
	r := keysRig(input)
	got, err := r.c.LineInput("Name: ")
	if err != nil {
		t.Fatalf("line input: %v", err)
	}
	return got, r
}

func TestLineInputSupportsCursorAndEmacsEditing(t *testing.T) {
	for keys, want := range map[string]string{
		"hello\r":                            "hello",
		"helo\x1b[Dl\r":                      "hello",
		"world\x01hello \r":                  "hello world",
		"abc\x1b[H\x1b[3~\r":                 "bc",
		"abc\x7f\x7fx\r":                     "ax",
		"one two\x17three\r":                 "one three",
		"abc\x01\x0b\x19\x19\r":              "abcabc",
		"abcdef\x01\x06\x06\x15\r":           "cdef",
		"ab\x14\r":                           "ba",
		"one two\x1bbX\r":                    "one Xtwo",
		"x\x1b[200~pasted\ntext\x1b[201~y\r": "xpasted texty",
		"caf\xc3\xa9\x7f\xc3\xa8\r":          "cafè",
	} {
		if got, _ := lineOf(t, keys); got != want {
			t.Fatalf("keys %q gave %q, want %q", keys, got, want)
		}
	}
}

func TestLineInputLeavesItsAnswerAndRestoresTheCursor(t *testing.T) {
	got, r := lineOf(t, "  icebath \r")
	if got != "  icebath " {
		t.Fatalf("got %q", got)
	}
	if r.text() != "  › Name  icebath" {
		t.Fatalf("screen:\n%q", r.text())
	}
	if !r.emu.Cursor().Visible || r.emu.BracketedPaste() {
		t.Fatal("cursor hidden or bracketed paste left on")
	}
}

func TestLineInputShowsTheDefaultAndKeepsItOnEnter(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.LineInputDefault("Max iterations [150]: ", "Max iterations", "150")
		done <- got
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "● Max iterations") && strings.Contains(s, "enter keeps 150")
	})
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != "" {
		t.Fatalf("got %q", got)
	}
	if r.text() != "  › Max iterations  150" {
		t.Fatalf("screen:\n%q", r.text())
	}
}

func TestLineInputInterruptAndEndOfInput(t *testing.T) {
	r := keysRig("abc\x03")
	if _, err := r.c.LineInput("Name: "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("ctrl c gave %v", err)
	}
	if !strings.Contains(r.text(), "› Name  cancelled") {
		t.Fatalf("screen:\n%s", r.text())
	}
	if _, err := keysRig("\x04").c.LineInput("Name: "); !errors.Is(err, ErrEOF) {
		t.Fatalf("ctrl d on an empty field gave %v", err)
	}
	if got, err := keysRig("ab\x01\x04\r").c.LineInput("Name: "); err != nil || got != "b" {
		t.Fatalf("ctrl d inside text gave %q %v", got, err)
	}
	if got, err := keysRig("partial").c.LineInput("Name: "); err != nil || got != "partial" {
		t.Fatalf("closed input with text gave %q %v", got, err)
	}
	if _, err := keysRig("").c.LineInput("Name: "); !errors.Is(err, ErrEOF) {
		t.Fatalf("closed input gave %v", err)
	}
}

func TestLineInputReadsPlainlyOffATerminal(t *testing.T) {
	var out bytes.Buffer
	c := &Console{In: strings.NewReader("typed\nrest"), Out: &out}
	got, err := c.LineInput("Name: ")
	if err != nil || got != "typed" || out.String() != "Name: " {
		t.Fatalf("got %q %v out %q", got, err, out.String())
	}
	r := dumb("typed\n")
	if got, err := r.c.LineInput("Name: "); err != nil || got != "typed" || !strings.Contains(r.text(), "Name:") {
		t.Fatalf("dumb terminal got %q %v:\n%s", got, err, r.text())
	}
}

func TestSecretLineMasksEveryCharacterAndReportsTheLength(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.SecretLine("  API key (sk-...): ", "*")
		done <- got
	}()
	io.WriteString(pipeOut, "sk-secret\x7f")
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "❯ ••••••••") && strings.Contains(s, "8 characters") && !strings.Contains(s, "secre")
	})
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != "sk-secre" {
		t.Fatalf("got %q", got)
	}
	if r.text() != "  › API key (sk-...)  •••••••• (8 characters)" {
		t.Fatalf("screen:\n%q", r.text())
	}
}

func TestSecretLineWithAnEmptyMaskShowsNothing(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.SecretLine("Token: ", "")
		done <- got
	}()
	io.WriteString(pipeOut, "hidden")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "6 characters") })
	if strings.Contains(r.text(), "•") || strings.Contains(r.text(), "hidden") {
		t.Fatalf("an empty mask drew the secret:\n%s", r.text())
	}
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != "hidden" {
		t.Fatalf("got %q", got)
	}
}

func TestSecretLineNeedsATerminalThatDraws(t *testing.T) {
	if _, err := dumb("x\n").c.SecretLine("Token: ", "*"); !errors.Is(err, ErrNoEditor) {
		t.Fatalf("got %v", err)
	}
}

func TestLongTextScrollsSidewaysInsideTheField(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 30, 10)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.LineInput("Path: ")
		done <- got
	}()
	text := "/a/very/long/path/that/does/not/fit/in/thirty/columns/tail"
	io.WriteString(pipeOut, text)
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "columns/tail") && !strings.Contains(s, "/a/very") })
	io.WriteString(pipeOut, "\x01")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "/a/very") && !strings.Contains(s, "columns/tail") })
	io.WriteString(pipeOut, "\r")
	if got := <-done; got != text {
		t.Fatalf("got %q", got)
	}
}

func setNav(t *testing.T, n *navLog) {
	t.Helper()
	tok := SetMenuNavigationHandler(n.handle)
	t.Cleanup(func() { ResetMenuNavigationHandler(tok) })
}

func TestScopedTextFieldReportsItsAnswerAndGoesBackWhenEmpty(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{AllowBack: true}}
	setNav(t, n)
	if got, err := keysRig("abc\x1b[D\x1b[Dx\r").c.LineInput("Name: "); err != nil || got != "axbc" {
		t.Fatalf("left arrow inside text gave %q %v", got, err)
	}
	if !reflect.DeepEqual(n.events, []string{"begin", "resolve"}) || !reflect.DeepEqual(n.values, []any{"Name", "axbc"}) {
		t.Fatalf("events %v values %v", n.events, n.values)
	}
	n.events, n.values = nil, nil
	if _, err := keysRig("\x1b[D").c.LineInput("Name: "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("left arrow on an empty field gave %v", err)
	}
	if !reflect.DeepEqual(n.events, []string{"begin", "back"}) {
		t.Fatalf("events %v", n.events)
	}
}

func TestScopedTextFieldCancelsOnTheSecondEscape(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{GuardCancel: true}}
	setNav(t, n)
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	done := make(chan string, 1)
	go func() {
		got, _ := r.c.LineInput("Name: ")
		done <- got
	}()
	io.WriteString(pipeOut, "ab\x1b")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "press esc again to cancel") })
	io.WriteString(pipeOut, "c\r")
	if got := <-done; got != "abc" {
		t.Fatalf("one escape then typing gave %q", got)
	}
	n.events = nil
	if _, err := keysRig("ab\x1b\x1b").c.LineInput("Name: "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("two escapes gave %v", err)
	}
	if !reflect.DeepEqual(n.events, []string{"begin", "cancel"}) {
		t.Fatalf("events %v", n.events)
	}
	n.start, n.events = MenuNavigationStart{}, nil
	if _, err := keysRig("ab\x1b").c.LineInput("Name: "); !errors.Is(err, ErrInterrupt) || !reflect.DeepEqual(n.events, []string{"begin", "cancel"}) {
		t.Fatalf("one escape without the guard gave %v events %v", err, n.events)
	}
}

func TestScopedTextFieldReplaysItsRecordedAnswer(t *testing.T) {
	n := &navLog{start: MenuNavigationStart{Replay: true, ReplayValue: "kept"}}
	setNav(t, n)
	r := keysRig("")
	if got, err := r.c.LineInput("Name: "); err != nil || got != "kept" {
		t.Fatalf("got %q %v", got, err)
	}
	if r.text() != "  › Name  kept" {
		t.Fatalf("screen:\n%q", r.text())
	}
	r = keysRig("")
	if got, _ := r.c.SecretLine("Key: ", "*"); got != "kept" || strings.Contains(r.text(), "kept") {
		t.Fatalf("secret replay got %q:\n%s", got, r.text())
	}
}

func TestUnusedKeysStayForTheNextPrompt(t *testing.T) {
	r := keysRig("\x1b[B\rtyped\r\x1b[B\x1b[B\r")
	if got := radio(t, r, 0, cancelAt(-1), "a", "b", "c"); got != 1 {
		t.Fatalf("first list picked %d", got)
	}
	if got, err := r.c.LineInput("Name: "); err != nil || got != "typed" {
		t.Fatalf("field got %q %v", got, err)
	}
	if got := radio(t, r, 0, cancelAt(-1), "a", "b", "c"); got != 2 {
		t.Fatalf("second list picked %d", got)
	}
}

func TestAnswersStackTightAndTextKeepsOneBlankLine(t *testing.T) {
	r := keysRig("\r\rname\r")
	flow := r.c.Flow()
	if !flow.IsFlow() || flow.Flow() != flow {
		t.Fatal("flow console")
	}
	io.WriteString(flow.Out, "\n\n\nIntro line\n")
	flow.CursesRadiolist(context.Background(), "First", items("a", "b"), 0, cancelAt(-1))
	io.WriteString(flow.Out, "\n")
	flow.CursesRadiolist(context.Background(), "Second", items("c", "d"), 0, cancelAt(-1))
	flow.LineInput("Third: ")
	io.WriteString(flow.Out, "\n\n   indented detail\n✓ saved\n\x1b[32m✓ green saved\x1b[0m\n⚠ careful\n✗ failed\n\n◆ Section\nbody\n◆ Next\n")
	want := strings.Join([]string{
		"  Intro line",
		"",
		"  › First  a",
		"  › Second  c",
		"  › Third  name",
		"",
		"   indented detail",
		"  ✓ saved",
		"  ✓ green saved",
		"  ⚠ careful",
		"  ✗ failed",
		"",
		"  ◆ Section",
		"  body",
		"",
		"  ◆ Next",
	}, "\n")
	if got := r.text(); got != want {
		t.Fatalf("flow output:\n%s\nwant:\n%s", got, want)
	}
}

func TestFlowWriterHandlesSplitWritesAndCarriageReturns(t *testing.T) {
	r := keysRig("")
	flow := r.c.Flow()
	for _, chunk := range []string{"par", "tial ", "li", "ne\n", "\x1b[2", "m  dim\x1b[0m\n", "caf\xc3", "\xa9\n", "spin 1\rspin 2\n"} {
		io.WriteString(flow.Out, chunk)
	}
	if got := r.text(); got != "  partial line\n  dim\n  café\n  spin 2" {
		t.Fatalf("got:\n%q", got)
	}
}

func TestFlowNestsStatusMarksRestylesRulesAndWrapsAtSpaces(t *testing.T) {
	r := newRig(strings.NewReader(""), 50, 30)
	flow := r.c.Flow()
	for _, line := range []string{
		"✓   Browser set to local mode",
		"✓     browser_exec ready",
		"   ✗ Web Search (missing EXA_API_KEY, PARALLEL_API_KEY, or SEARXNG_URL)",
		"--- 🌐 Browser Automation - Choose a provider ---",
		"  ─── Keys ───",
		"☤ Icebath Tool Configuration",
		"Guide: https://example.com/a/very-long/path-that-must-stay/in-one-piece#anchor-part",
		"Without a service, scheduled jobs and messaging stop at logout",
		"━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		"│ a box row that is wider than the terminal stays as it was printed │",
	} {
		io.WriteString(flow.Out, line+"\n")
	}
	flow.Done("Setup complete")
	want := strings.Join([]string{
		"    ✓ Browser set to local mode",
		"      ✓ browser_exec ready",
		"   ✗ Web Search (missing EXA_API_KEY,",
		"     PARALLEL_API_KEY, or SEARXNG_URL)",
		"",
		"  ━━ 🌐 Browser Automation - Choose a provider",
		"",
		"  ━━ Keys",
		"",
		"  ◆ Icebath Tool Configuration",
		"  Guide:",
		"  https://example.com/a/very-long/path-that-must-s",
		"tay/in-one-piece#anchor-part",
		"  Without a service, scheduled jobs and messaging",
		"  stop at logout",
		"  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
		"  │ a box row that is wider than the terminal stay",
		"s as it was printed │",
		"",
		"  ━━━━━━╱━━━  ✓ Setup complete",
	}, "\n")
	if got := r.text(); got != want {
		t.Fatalf("flow output:\n%s\nwant:\n%s", got, want)
	}
}

func TestFlowSwapsBasicColorsForPaletteTones(t *testing.T) {
	var out bytes.Buffer
	r := keysRig("")
	r.c.Out = &out
	r.env["COLORTERM"] = "truecolor"
	flow := r.c.Flow()
	io.WriteString(flow.Out, "\x1b[36m\x1b[1mTo edit\x1b[0m \x1b[32micebath setup\x1b[0m \x1b[38;5;36mkept\x1b[0m \x1b[2mdim\x1b[0m\n")
	want := "  \x1b[38;2;155;220;247m\x1b[1mTo edit\x1b[0m \x1b[38;2;98;211;148micebath setup\x1b[0m \x1b[38;5;36mkept\x1b[0m \x1b[2mdim\x1b[0m\n"
	if out.String() != want {
		t.Fatalf("got %q", out.String())
	}
}

func TestInputInsideAFlowIsAYesNoListOrAField(t *testing.T) {
	r := keysRig("\ry")
	flow := r.c.Flow()
	if got, err := flow.Input("Log in to Nous Portal now? [Y/n]: "); err != nil || got != "y" {
		t.Fatalf("enter on a default yes gave %q %v", got, err)
	}
	if got, err := flow.Input("Switch anyway? [y/N]: "); err != nil || got != "y" {
		t.Fatalf("the y key gave %q %v", got, err)
	}
	if got, err := keysRig("\r").c.Flow().Input("Overwrite (y/N) "); err != nil || got != "n" {
		t.Fatalf("enter on a default no gave %q %v", got, err)
	}
	if _, err := keysRig("\x1b").c.Flow().Input("Proceed? [Y/n]: "); !errors.Is(err, ErrInterrupt) {
		t.Fatalf("escape gave %v", err)
	}
	field := keysRig("typed\r")
	if got, err := field.c.Flow().Input("Name: "); err != nil || got != "typed" || field.text() != "  › Name  typed" {
		t.Fatalf("field got %q %v:\n%s", got, err, field.text())
	}
	var out bytes.Buffer
	plain := &Console{In: strings.NewReader("y\n"), Out: &out}
	if got, err := plain.Input("Proceed? [Y/n]: "); err != nil || got != "y" || out.String() != "Proceed? [Y/n]: " {
		t.Fatalf("outside a flow got %q %v out %q", got, err, out.String())
	}
}

func TestLongTitleWrapsWhileLiveAndInItsAnswer(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 50, 20)
	done := make(chan struct{})
	title := "Install the gateway service so Icebath starts automatically at login?"
	go func() {
		r.c.CursesRadiolist(context.Background(), title, items("Yes", "No"), 1, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "● Install the gateway service so Icebath starts\n    automatically at login?")
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if got := r.text(); got != "  › Install the gateway service so Icebath starts\n    automatically at login?  No" {
		t.Fatalf("answer rows:\n%q", got)
	}
}

func TestBracketedNotesGetTheirOwnColumn(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 20)
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Choose a provider", items("Local Browser [★ recommended · free] - Headless Chromium",
			"Browserbase [paid] - Cloud browser", "Skip - keep defaults"), 0, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "❯ Local Browser  ★ recommended · free  Headless Chromium") &&
			strings.Contains(s, "  Browserbase    paid                  Cloud browser") &&
			strings.Contains(s, "  Skip                                 keep defaults")
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if got := r.text(); got != "  › Choose a provider  Local Browser" {
		t.Fatalf("answer row:\n%q", got)
	}
}

func TestLargeSelectionIsSummarizedByItsCount(t *testing.T) {
	r := keysRig("\r")
	r.c.CursesChecklist(context.Background(), "Tools", []string{"a", "b", "c", "d"}, map[int]bool{0: true, 1: true, 2: true, 3: true}, nil, nil)
	if got := r.text(); got != "  › Tools  4 selected" {
		t.Fatalf("answer row:\n%q", got)
	}
}

func TestFlowIsTheSameConsoleOffATerminal(t *testing.T) {
	c := &Console{In: strings.NewReader(""), Out: &bytes.Buffer{}}
	if c.Flow() != c || c.IsFlow() {
		t.Fatal("a buffer console became a flow")
	}
	if d := dumb(""); d.c.Flow() != d.c {
		t.Fatal("a dumb terminal became a flow")
	}
}

func TestBannerPicksTheArtThatFitsAndStepNumbersItsRule(t *testing.T) {
	r := keysRig("")
	flow := r.c.Flow()
	flow.Banner([]string{"WIDE ART ROW ONE", "WIDE ART ROW TWO"}, "compact", "Setup")
	flow.Step(2, 4, "Terminal Backend")
	flow.Step(1, 1, "Agent Settings")
	want := "\n  WIDE ART ROW ONE\n  WIDE ART ROW TWO\n\n  Setup\n\n  ━━━━━━╱━━━  2 / 4  Terminal Backend\n\n  ━━━━━━╱━━━  Agent Settings"
	if got := r.text(); got != want {
		t.Fatalf("got:\n%q", got)
	}
	narrow := newRig(strings.NewReader(""), 12, 10)
	narrow.c.Flow().Banner([]string{"WIDE ART ROW ONE"}, "compact", "")
	if got := narrow.text(); got != "\n  compact" {
		t.Fatalf("narrow banner:\n%q", got)
	}
}

func TestAwaitSpinsUntilOutputArrives(t *testing.T) {
	r := keysRig("")
	flow := r.c.Flow()
	stop := flow.Await("Checking providers")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "Checking providers") })
	io.WriteString(flow.Out, "first line\n")
	time.Sleep(200 * time.Millisecond)
	if got := r.text(); got != "  first line" {
		t.Fatalf("spinner outlived the output:\n%q", got)
	}
	stop()
	stop()
	if !r.emu.Cursor().Visible {
		t.Fatal("cursor hidden after the spinner")
	}
	r = keysRig("\r")
	flow = r.c.Flow()
	stop = flow.Await("Loading")
	waitFor(t, r, func(s string) bool { return strings.Contains(s, "Loading") })
	flow.CursesRadiolist(context.Background(), "Pick", items("a"), 0, cancelAt(-1))
	stop()
	if got := r.text(); got != "  › Pick  a" {
		t.Fatalf("spinner outlived the prompt:\n%q", got)
	}
	plain := keysRig("")
	plain.c.Await("Nothing")()
	if plain.text() != "" {
		t.Fatalf("a console outside a flow drew a spinner:\n%q", plain.text())
	}
}

func TestASCIITerminalDrawsWithoutUnicodeMarks(t *testing.T) {
	pipeIn, pipeOut := io.Pipe()
	r := newRig(pipeIn, 100, 30)
	r.env["LANG"] = "C"
	done := make(chan struct{})
	go func() {
		r.c.CursesRadiolist(context.Background(), "Pick", items("a", "b"), 0, cancelAt(-1))
		close(done)
	}()
	waitFor(t, r, func(s string) bool {
		return strings.Contains(s, "* Pick") && strings.Contains(s, "> a") && strings.Contains(s, "up/down move")
	})
	io.WriteString(pipeOut, "\r")
	<-done
	if r.text() != "  > Pick  a" {
		t.Fatalf("screen:\n%q", r.text())
	}
}

func TestColorDepthFollowsTheTerminal(t *testing.T) {
	r := keysRig("")
	if got := r.c.Tint("x", "#f50325", true); got != "\x1b[1;38;5;196mx\x1b[0m" {
		t.Fatalf("256 colors: %q", got)
	}
	r.env["COLORTERM"] = "truecolor"
	if got := r.c.Tint("x", "#f50325", false); got != "\x1b[38;2;245;3;37mx\x1b[0m" {
		t.Fatalf("truecolor: %q", got)
	}
	delete(r.env, "COLORTERM")
	r.env["TERM"] = "xterm"
	if got := r.c.Tint("x", "#f50325", false); got != "\x1b[91mx\x1b[0m" {
		t.Fatalf("16 colors: %q", got)
	}
	r.env["NO_COLOR"] = "1"
	if got := r.c.Tint("x", "#f50325", true); got != "\x1b[1mx\x1b[0m" {
		t.Fatalf("no color keeps bold only: %q", got)
	}
	r.env["TERM"] = "dumb"
	if got := r.c.Tint("x", "#f50325", true); got != "x" {
		t.Fatalf("dumb: %q", got)
	}
}

func TestReconcileCursorMovesToFirstVisibleMatch(t *testing.T) {
	if c, p := ReconcileCursor([]int{2, 4}, 0); c != 2 || p != 0 {
		t.Fatal("first match")
	}
	if c, p := ReconcileCursor([]int{2, 4}, 4); c != 4 || p != 1 {
		t.Fatal("kept cursor")
	}
}

func TestScorerRanksPrefixAndBoundaryMatchesHigher(t *testing.T) {
	a, _ := FuzzyScore("gpt-4o", "gpt")
	b, _ := FuzzyScore("gpt-4o", "g4o")
	c, _ := FuzzyScore("claude-sonnet-4", "sonnet")
	d, _ := FuzzyScore("claude-sonnet-4", "clad snnt")
	if !(a > b) || !(c > d) {
		t.Fatalf("scores %v %v %v %v", a, b, c, d)
	}
	if _, ok := FuzzyScore("gpt-4o", "zzz"); ok {
		t.Fatal("non subsequence matched")
	}
}

func bar(fraction float64) string {
	n := int(fraction*10 + 0.5)
	return "[" + strings.Repeat("|", n) + strings.Repeat(" ", 10-n) + "]"
}

func TestRealGaugesAreStyled(t *testing.T) {
	for _, tc := range []struct {
		f     float64
		style string
	}{{0, "green"}, {0.6, "yellow"}, {0.9, "red"}} {
		b := bar(tc.f)
		want := []Segment{{Text: "Today  "}, {Text: b, Style: tc.style}, {Text: "  x"}}
		if got := GaugeBarSegments("Today  " + b + "  x"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v got %v", tc.f, got)
		}
	}
}

func TestUnrelatedBracketsStayPlain(t *testing.T) {
	for _, text := range []string{"toggle [] here", "checkbox [ ] off", "a [||] b", "flag [x]", "[" + strings.Repeat(" ", 11) + "]", "[| |       ]"} {
		if got := GaugeBarSegments(text); !reflect.DeepEqual(got, []Segment{{Text: text}}) {
			t.Fatalf("%q got %v", text, got)
		}
	}
}

func TestColorizeAndFormatANSI(t *testing.T) {
	c := keysRig("").c
	if got := c.ColorizeGaugeBarsANSI("x " + bar(0.9)); got != "x "+Red+bar(0.9)+Reset {
		t.Fatalf("got %q", got)
	}
	item := RadioItem{{Text: "a"}, {Text: "b", Style: "dim"}, {Text: "c", Style: "bogus"}}
	if got := c.FormatRadioItemANSI(item); got != "a"+Dim+"b"+Reset+"c" {
		t.Fatalf("got %q", got)
	}
	c.Getenv = func(k string) (string, bool) { return "", k == "NO_COLOR" }
	if c.Color("x", Red) != "x" {
		t.Fatal("NO_COLOR ignored")
	}
}

func TestPromptAndYesNoReadThroughTheField(t *testing.T) {
	r := keysRig("  typed  \r\r")
	if got := r.c.Prompt("Name", "", false); got != "typed" {
		t.Fatalf("got %q", got)
	}
	if got := r.c.Prompt("Name", "fallback", false); got != "fallback" {
		t.Fatalf("default got %q", got)
	}
	r = keysRig("")
	r.c.MaskedSecretPrompt = func(prompt string) (string, error) { return " secret ", nil }
	if got := r.c.Prompt("Key", "", true); got != "secret" {
		t.Fatalf("password got %q", got)
	}
	if got := keysRig("\x03").c.Prompt("Name", "x", false); got != "" {
		t.Fatalf("interrupt got %q", got)
	}
	if !keysRig("yes\r").c.PromptYesNo(context.Background(), "Go", false) || keysRig("\r").c.PromptYesNo(context.Background(), "Go", false) {
		t.Fatal("yes no answers")
	}
}

func TestFlushStdinIsTTYGated(t *testing.T) {
	c := &Console{In: strings.NewReader("leftover\n"), Out: &bytes.Buffer{}}
	c.FlushStdin()
	if got, _ := c.input(""); got != "leftover" {
		t.Fatalf("non tty flush consumed %q", got)
	}
}
