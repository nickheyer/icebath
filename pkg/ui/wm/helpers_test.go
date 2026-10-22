package wm

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Card that records every message it receives
type stub struct {
	title    string
	key      string
	float    bool
	sel      bool
	body     string
	msgs     []tea.Msg
	focused  bool
	inits    int
	lastSize [2]int
}

func newStub(title string) *stub { return &stub{title: title, key: title, body: title} }

func (s *stub) Title() string { return s.title }
func (s *stub) Key() string   { return s.key }
func (s *stub) Init() tea.Cmd { s.inits++; return nil }

func (s *stub) Update(msg tea.Msg) (Card, tea.Cmd) {
	s.msgs = append(s.msgs, msg)
	if f, ok := msg.(FocusMsg); ok {
		s.focused = f.Focused
	}
	return s, nil
}

func (s *stub) View(w, h int) string {
	s.lastSize = [2]int{w, h}
	lines := strings.Split(s.body, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines[:h], "\n")
}

func (s *stub) Floating() bool   { return s.float }
func (s *stub) Selectable() bool { return s.sel }

// Messages of one type the stub received
func received[T any](s *stub) []T {
	var out []T
	for _, m := range s.msgs {
		if v, ok := m.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// Stub the close command leaves alone
type pinned struct{ *stub }

func (p pinned) Closable() bool { return false }

func (p pinned) Update(msg tea.Msg) (Card, tea.Cmd) {
	p.stub.Update(msg)
	return p, nil
}

// Stub that places a cursor and declares a size
type sized struct {
	*stub
	w, h int
}

func (s sized) MinSize() (int, int) { return s.w, s.h }
func (s sized) Cursor() *tea.Cursor { return tea.NewCursor(1, 2) }

func (s sized) Update(msg tea.Msg) (Card, tea.Cmd) {
	s.stub.Update(msg)
	return s, nil
}

// Manager at a fixed size with motion off and a clock that steps one second per reading
func still(t *testing.T, w, h int, opts ...Option) *Model {
	t.Helper()
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	tick := func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	all := append([]Option{WithMotion(0), WithClock(tick)}, opts...)
	m := New(all...)
	m.SetSize(w, h)
	return m
}

// Clock that never moves, so every click lands inside the double click window
func frozen() Option {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return WithClock(func() time.Time { return now })
}

// Manager with one workspace holding master and two stack tiles at 100 by 30, gamma opened first so beta lands above it
func threeTiles(t *testing.T, opts ...Option) (*Model, *stub, *stub, *stub) {
	t.Helper()
	m := still(t, 100, 30, opts...)
	a, b, c := newStub("alpha"), newStub("beta"), newStub("gamma")
	m.AddWorkspace("ws1", a)
	m.OpenHere(c)
	m.OpenHere(b)
	m.Focus(a)
	return m, a, b, c
}

// Advances the animation until nothing moves, failing when it never settles
func settle(t *testing.T, m *Model) int {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if !m.Advance(1.0 / 60) {
			return i
		}
	}
	t.Fatal("animation never settled")
	return 0
}

func key(code rune, mods ...tea.KeyMod) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Code: code}
	if unicode.IsPrint(code) {
		k.Text = string(code)
	}
	for _, m := range mods {
		k.Mod |= m
	}
	if k.Mod != 0 {
		k.Text = ""
	}
	return k
}

func click(x, y int, b tea.MouseButton) tea.MouseClickMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: b}
}

func move(x, y int, b tea.MouseButton) tea.MouseMotionMsg {
	return tea.MouseMotionMsg{X: x, Y: y, Button: b}
}

func release(x, y int, b tea.MouseButton) tea.MouseReleaseMsg {
	return tea.MouseReleaseMsg{X: x, Y: y, Button: b}
}

// Runs a command tree, gathering every message that arrives within the wait
func collect(cmd tea.Cmd, wait time.Duration) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 64)
	var wg sync.WaitGroup
	var run func(c tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, inner := range batch {
					run(inner)
				}
				return
			}
			if msg != nil {
				ch <- msg
			}
		}()
	}
	run(cmd)
	var out []tea.Msg
	deadline := time.After(wait)
	for {
		select {
		case msg := <-ch:
			out = append(out, msg)
		case <-deadline:
			return out
		}
	}
}

// Runs a command tree and feeds every message back into the model
func pump(m *Model, cmd tea.Cmd) {
	for _, msg := range collect(cmd, 50*time.Millisecond) {
		m.Update(msg)
	}
}

func typeName(msg tea.Msg) string { return fmt.Sprintf("%T", msg) }

func plainTheme() theme.Theme { return theme.Plain() }
