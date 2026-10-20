package main

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Command line that captures typing while armed and echoes submitted lines above itself
type uplink struct {
	in      textinput.Model
	log     []string
	focused bool
	armed   bool
	h       int
	pal     theme.Theme
}

func newUplink() *uplink {
	pal := theme.Blackwall()
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "jack in"
	in.SetVirtualCursor(false)
	st := textinput.DefaultDarkStyles()
	st.Focused.Prompt = st.Focused.Prompt.Foreground(pal.Accent)
	st.Focused.Text = st.Focused.Text.Foreground(pal.Text)
	st.Focused.Placeholder = st.Focused.Placeholder.Foreground(pal.TextDim)
	st.Blurred.Prompt = st.Blurred.Prompt.Foreground(pal.TextDim)
	st.Blurred.Text = st.Blurred.Text.Foreground(pal.TextDim)
	st.Blurred.Placeholder = st.Blurred.Placeholder.Foreground(pal.TextDim)
	in.SetStyles(st)
	return &uplink{in: in, pal: pal}
}

var (
	sendHint    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send"))
	releaseHint = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "release"))
	jackInHint  = key.NewBinding(key.WithKeys("click"), key.WithHelp("click", "jack in"))
)

func (u *uplink) Title() string   { return "UPLINK" }
func (u *uplink) Glyph() string   { return "⌁" }
func (u *uplink) Init() tea.Cmd   { return nil }
func (u *uplink) Capturing() bool { return u.focused && u.armed }

// Keys for sending and releasing while armed, or the click that arms typing again
func (u *uplink) Hints() []key.Binding {
	if u.Capturing() {
		return []key.Binding{sendHint, releaseHint}
	}
	return []key.Binding{jackInHint}
}

func (u *uplink) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case deck.FocusMsg:
		u.focused = msg.Focused
		return u, u.arm(msg.Focused)
	case tea.MouseClickMsg:
		if u.focused && !u.armed {
			return u, u.arm(true)
		}
		return u, nil
	case tea.KeyPressMsg:
		switch {
		case msg.Code == tea.KeyEscape:
			return u, u.arm(false)
		case msg.Code == tea.KeyEnter:
			if v := strings.TrimSpace(u.in.Value()); v != "" {
				u.log = append(u.log, v)
				u.in.Reset()
			}
			return u, nil
		case !u.armed:
			armed := u.arm(true)
			var typed tea.Cmd
			u.in, typed = u.in.Update(msg)
			return u, tea.Batch(armed, typed)
		}
	}
	var cmd tea.Cmd
	u.in, cmd = u.in.Update(msg)
	return u, cmd
}

// Switches typing on or off and moves the input focus with it
func (u *uplink) arm(on bool) tea.Cmd {
	u.armed = on
	if on {
		return u.in.Focus()
	}
	u.in.Blur()
	return nil
}

func (u *uplink) View(w, h int) string {
	u.h = h
	u.in.SetWidth(max(1, w-lipgloss.Width(u.in.Prompt)))
	echo := lipgloss.NewStyle().Foreground(u.pal.TextDim)
	lines := make([]string, 0, h)
	start := max(0, len(u.log)-(h-1))
	for _, l := range u.log[start:] {
		lines = append(lines, echo.Render("» "+l))
	}
	for len(lines) < h-1 {
		lines = append([]string{""}, lines...)
	}
	lines = append(lines, u.in.View())
	return strings.Join(lines, "\n")
}

func (u *uplink) Cursor() *tea.Cursor {
	if !u.Capturing() {
		return nil
	}
	c := u.in.Cursor()
	if c == nil {
		return nil
	}
	c.Y += u.h - 1
	return c
}

func (u *uplink) Status() string {
	if u.Capturing() {
		return "live"
	}
	return ""
}
