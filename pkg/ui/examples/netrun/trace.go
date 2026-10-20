package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type traceTick time.Time

// Streaming log that follows its tail until the reader scrolls away
type trace struct {
	vp     viewport.Model
	lines  []string
	follow bool
	rng    *rand.Rand
	w, h   int
	pal    theme.Theme
}

var (
	verbs   = []string{"probe", "handshake", "spoof", "trace", "breach", "ping", "decrypt", "mask", "tunnel", "scrub"}
	results = []string{"ok", "ok", "ok", "ICE-II", "denied", "timeout", "routed"}
	ports   = []int{22, 443, 8080, 31337, 9001}
)

func newTrace() *trace {
	vp := viewport.New()
	vp.MouseWheelEnabled = true
	vp.SoftWrap = true
	return &trace{vp: vp, follow: true, rng: rand.New(rand.NewPCG(7, 11)), pal: theme.Blackwall()}
}

var (
	scrollHint = key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/pgdn", "scroll"))
	endsHint   = key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/end"))
)

func (t *trace) Title() string { return "TRACE" }
func (t *trace) Glyph() string { return "≋" }
func (t *trace) Init() tea.Cmd { return t.schedule() }

func (t *trace) Hints() []key.Binding { return []key.Binding{scrollHint, endsHint} }

func (t *trace) schedule() tea.Cmd {
	d := time.Duration(200+t.rng.IntN(600)) * time.Millisecond
	return tea.Tick(d, func(at time.Time) tea.Msg { return traceTick(at) })
}

func (t *trace) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case traceTick:
		t.lines = append(t.lines, t.line(time.Time(msg)))
		if len(t.lines) > 500 {
			t.lines = t.lines[len(t.lines)-500:]
		}
		t.refresh()
		return t, t.schedule()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "end", "G":
			t.vp.GotoBottom()
		case "home", "g":
			t.vp.GotoTop()
		default:
			var cmd tea.Cmd
			t.vp, cmd = t.vp.Update(msg)
			t.follow = t.vp.AtBottom()
			return t, cmd
		}
		t.follow = t.vp.AtBottom()
		return t, nil
	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		t.vp, cmd = t.vp.Update(msg)
		t.follow = t.vp.AtBottom()
		return t, cmd
	}
	return t, nil
}

func (t *trace) View(w, h int) string {
	if w != t.w || h != t.h {
		t.w, t.h = w, h
		t.vp.SetWidth(w)
		t.vp.SetHeight(h)
		t.refresh()
	}
	return t.vp.View()
}

// Line count, marking when the tail is held back by scrolling
func (t *trace) Status() string {
	if t.follow {
		return fmt.Sprintf("%d lines", len(t.lines))
	}
	return fmt.Sprintf("%d lines · paused", len(t.lines))
}

func (t *trace) refresh() {
	t.vp.SetContent(strings.Join(t.lines, "\n"))
	if t.follow {
		t.vp.GotoBottom()
	}
}

func (t *trace) line(at time.Time) string {
	p := t.pal
	res := results[t.rng.IntN(len(results))]
	col := p.Alive
	switch res {
	case "ICE-II", "denied":
		col = p.Alarm
	case "timeout":
		col = p.Accent
	case "routed":
		col = p.Ice
	}
	host := fmt.Sprintf("10.%d.%d.%d:%d", t.rng.IntN(255), t.rng.IntN(255), 1+t.rng.IntN(254), ports[t.rng.IntN(len(ports))])
	return lipgloss.NewStyle().Foreground(p.TextDim).Render(at.Format("15:04:05")) + " " +
		lipgloss.NewStyle().Foreground(p.Pulse).Render(verbs[t.rng.IntN(len(verbs))]) + " " +
		lipgloss.NewStyle().Foreground(p.Text).Render(host) + " " +
		lipgloss.NewStyle().Foreground(col).Render(res)
}
