package cards

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/render"
)

// Stored sessions to resume, fork, delete or search
type Sessions struct {
	env       *Env
	all       []session.Info
	rows      []session.Info
	sel       int
	query     string
	searching bool
	confirm   string
	err       error
	w, h      int
}

var (
	sResume = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "resume"))
	sFork   = key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "fork"))
	sDelete = key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete"))
	sSearch = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search"))
	sReload = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reload"))
)

func NewSessions(env *Env) *Sessions {
	s := &Sessions{env: env}
	s.Reload()
	return s
}

func (s *Sessions) Title() string   { return "SESSIONS" }
func (s *Sessions) Glyph() string   { return "⧉" }
func (s *Sessions) Key() string     { return "sessions" }
func (s *Sessions) Init() tea.Cmd   { return nil }
func (s *Sessions) Capturing() bool { return s.searching || s.confirm != "" }
func (s *Sessions) Hints() []key.Binding {
	return []key.Binding{sResume, sFork, sDelete, sSearch, sReload}
}
func (s *Sessions) Claims() []string {
	return []string{"j", "k", "g", "G", "enter", "f", "d", "/", "r"}
}

// Sessions shown after filtering
func (s *Sessions) Rows() []session.Info { return s.rows }

// Reads the listing from the store again
func (s *Sessions) Reload() {
	if s.env.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.all, s.err = s.env.Store.List(ctx)
	s.filter()
}

func (s *Sessions) filter() {
	s.rows = s.rows[:0]
	q := strings.ToLower(s.query)
	for _, info := range s.all {
		if q == "" || strings.Contains(strings.ToLower(info.ID+" "+info.Title+" "+info.Model), q) {
			s.rows = append(s.rows, info)
		}
	}
	if s.sel >= len(s.rows) {
		s.sel = max(0, len(s.rows)-1)
	}
}

// Puts the saved session at the top of the listing without reading the store
func (s *Sessions) saved(info session.Info) {
	kept := s.all[:0]
	for _, have := range s.all {
		if have.ID != info.ID {
			kept = append(kept, have)
		}
	}
	s.all = append([]session.Info{info}, kept...)
	s.filter()
}

func (s *Sessions) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.SavedMsg:
		if msg.Err == nil {
			s.saved(msg.Info)
		}
		return s, nil
	case deck.FocusMsg:
		if msg.Focused {
			s.Reload()
		}
		return s, nil
	case tea.KeyPressMsg:
		if s.confirm != "" {
			id := s.confirm
			s.confirm = ""
			if msg.String() == "y" || msg.String() == "Y" {
				return s, func() tea.Msg { return DeleteMsg{Session: id} }
			}
			return s, nil
		}
		if s.searching {
			switch msg.String() {
			case "enter":
				s.searching = false
			case "esc":
				s.searching = false
				s.query = ""
			case "backspace":
				if len(s.query) > 0 {
					s.query = s.query[:len(s.query)-1]
				}
			default:
				if msg.Text != "" {
					s.query += msg.Text
				}
			}
			s.filter()
			return s, nil
		}
		switch msg.String() {
		case "j", "down":
			s.sel = min(s.sel+1, max(0, len(s.rows)-1))
		case "k", "up":
			s.sel = max(0, s.sel-1)
		case "g":
			s.sel = 0
		case "G":
			s.sel = max(0, len(s.rows)-1)
		case "/":
			s.searching = true
			s.query = ""
		case "r":
			s.Reload()
		case "enter":
			if s.sel < len(s.rows) {
				id := s.rows[s.sel].ID
				return s, func() tea.Msg { return ResumeMsg{Session: id} }
			}
		case "f":
			if s.sel < len(s.rows) {
				id := s.rows[s.sel].ID
				return s, func() tea.Msg { return ForkMsg{Session: id} }
			}
		case "d":
			if s.sel < len(s.rows) {
				s.confirm = s.rows[s.sel].ID
			}
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y >= 2 && msg.Y-2 < len(s.rows) {
			s.sel = msg.Y - 2
		}
	}
	return s, nil
}

func (s *Sessions) View(w, h int) string {
	s.w, s.h = w, h
	t := s.env.Theme
	var lines []string
	head := render.Dim(fmt.Sprintf("%d sessions", len(s.rows)), t)
	switch {
	case s.confirm != "":
		head = render.Bad("delete "+s.confirm+"? y/n", t)
	case s.searching || s.query != "":
		head += " " + render.Warm("/"+s.query, t)
	}
	if s.err != nil {
		head += " " + render.Bad(s.err.Error(), t)
	}
	lines = append(lines, render.Truncate(head, w))
	if len(s.rows) == 0 {
		return strings.Join(append(lines, "", render.Dim("nothing stored", t)), "\n")
	}
	current := ""
	if s.env.Session != nil {
		if sess := s.env.Session(); sess != nil {
			current = sess.ID()
		}
	}
	cols := []render.Column{{Title: ""}, {Title: "UPDATED"}, {Title: "TITLE", Flex: 1}, {Title: "MODEL", Max: 24}, {Title: "MSGS", Right: true}, {Title: "TOKENS", Right: true}}
	rows := make([][]string, 0, len(s.rows))
	for _, info := range s.rows {
		mark := " "
		if info.ID == current {
			mark = render.Cool("●", t)
		}
		title := info.Title
		if title == "" {
			title = info.ID
		}
		if info.Parent != "" {
			title += render.Dim(" ⑂", t)
		}
		if info.Error != "" {
			title = render.Bad(info.ID+" "+info.Error, t)
		}
		rows = append(rows, []string{mark, info.Updated.Local().Format("01-02 15:04"), title, info.Model, fmt.Sprint(info.Messages), render.Tokens(info.Usage.Total())})
	}
	table := strings.Split(render.Table(cols, rows, w, s.sel, t), "\n")
	if len(table) > h-1 {
		start := 0
		if s.sel+1 >= h-1 {
			start = s.sel + 2 - (h - 1)
		}
		table = append(table[:1], table[1+start:min(len(table), 1+start+h-2)]...)
	}
	return strings.Join(append(lines, table...), "\n")
}

func (s *Sessions) Status() string {
	if s.searching {
		return "search"
	}
	return fmt.Sprintf("%d", len(s.rows))
}
