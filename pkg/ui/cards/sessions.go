package cards

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Stored sessions to resume, fork or delete one or many at a time, narrowed by a filter bar along the bottom
//
// The listing shows the sessions of this project unless widened to every project, and marks the sessions another
// icebath has open since those cannot be resumed here
type Sessions struct {
	env     *Env
	all     []session.Info
	rows    []session.Info
	sel     int
	anchor  int
	bar     searchBar
	confirm []string
	err     error
	top     int
	start   int
	w, h    int
	// Whether sessions of every project are shown rather than those of this one
	every bool
}

func NewSessions(env *Env) *Sessions {
	s := &Sessions{env: env, anchor: -1}
	s.Reload()
	return s
}

// Shows sessions of every project or of this one alone
func (s *Sessions) SetEvery(on bool) {
	s.every = on
	s.filter()
}

// Reports whether sessions of every project are shown
func (s *Sessions) Every() bool { return s.every }

func (s *Sessions) Title() string { return "SESSIONS" }
func (s *Sessions) Glyph() string { return s.env.Theme.Glyphs.Sessions }
func (s *Sessions) Key() string   { return "sessions" }
func (s *Sessions) Init() tea.Cmd { return nil }

// Keymap contexts in force, a pending question or the filter bar ahead of the table
func (s *Sessions) Contexts() []string {
	switch {
	case len(s.confirm) > 0:
		return []string{keys.Confirm}
	case s.bar.Active():
		return s.bar.Contexts()
	}
	return []string{keys.Sessions, keys.Filter, keys.List, keys.Panel}
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

// Reports whether a session belongs to this project, those of no project and those open here counting as ours
func (s *Sessions) ours(info session.Info, open map[string]bool) bool {
	project := info.Labels[session.ProjectLabel]
	return project == "" || s.env.Root == "" || project == s.env.Root || open[info.ID]
}

func (s *Sessions) filter() {
	s.rows = s.rows[:0]
	q := strings.ToLower(s.bar.Query())
	open := s.open()
	for _, info := range s.all {
		if !s.every && !s.ours(info, open) {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(info.ID+" "+info.Title+" "+info.Model+" "+info.Labels[session.ProjectLabel]), q) {
			s.rows = append(s.rows, info)
		}
	}
	if s.sel >= len(s.rows) {
		s.sel = max(0, len(s.rows)-1)
	}
	if s.anchor >= len(s.rows) {
		s.anchor = -1
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

// Ids of the sessions open in a workspace
func (s *Sessions) open() map[string]bool {
	out := map[string]bool{}
	if s.env.Lives == nil {
		return out
	}
	for _, l := range s.env.Lives() {
		out[l.Session.ID()] = true
	}
	return out
}

// First and last row of the selection, the cursor row alone unless a range was extended from an anchor
func (s *Sessions) span() (int, int) {
	if s.anchor < 0 {
		return s.sel, s.sel
	}
	return min(s.anchor, s.sel), max(s.anchor, s.sel)
}

// Reports whether row i is selected
func (s *Sessions) marked(i int) bool {
	lo, hi := s.span()
	return i >= lo && i <= hi
}

// Sessions selected, in listing order
func (s *Sessions) Selected() []session.Info {
	if len(s.rows) == 0 {
		return nil
	}
	lo, hi := s.span()
	return append([]session.Info(nil), s.rows[lo:min(hi+1, len(s.rows))]...)
}

func (s *Sessions) ids() []string {
	var out []string
	for _, info := range s.Selected() {
		out = append(out, info.ID)
	}
	return out
}

// Moves the cursor to row, extending the selection from where it stood when extend is set and collapsing it otherwise
func (s *Sessions) moveTo(row int, extend bool) {
	if len(s.rows) == 0 {
		return
	}
	if extend && s.anchor < 0 {
		s.anchor = s.sel
	}
	if !extend {
		s.anchor = -1
	}
	s.sel = min(max(row, 0), len(s.rows)-1)
}

func (s *Sessions) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case harness.SavedMsg:
		if msg.Err == nil {
			s.saved(msg.Info)
		}
		return s, nil
	case wm.FocusMsg:
		if msg.Focused {
			s.Reload()
		} else {
			s.bar.Accept()
			s.confirm = nil
		}
		return s, nil
	case wm.ActionMsg:
		return s, s.act(msg.ID)
	case tea.KeyPressMsg:
		if s.bar.Active() && msg.Text != "" {
			s.bar.Type(msg.Text)
			s.filter()
		}
		return s, nil
	case tea.PasteMsg:
		if s.bar.Active() {
			s.bar.Type(msg.Content)
			s.filter()
		}
		return s, nil
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return s, nil
		}
		if msg.Y == s.h-1 && s.bar.Shown() && len(s.confirm) == 0 {
			s.bar.Open()
			return s, nil
		}
		s.bar.Accept()
		if row := msg.Y - s.top - render.TableHead + s.start; msg.Y >= s.top+render.TableHead && row < len(s.rows) {
			s.moveTo(row, msg.Mod.Contains(tea.ModShift))
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelDown:
			s.moveTo(s.sel+1, false)
		case tea.MouseWheelUp:
			s.moveTo(s.sel-1, false)
		}
	}
	return s, nil
}

// Carries out a keymap action on the pending question, the filter bar or the selection
func (s *Sessions) act(id string) tea.Cmd {
	page := s.h - s.top - render.TableHead
	switch id {
	case "confirm.yes":
		ids := s.confirm
		s.confirm = nil
		s.anchor = -1
		return func() tea.Msg { return DeleteMsg{Sessions: ids} }
	case "confirm.no":
		s.confirm = nil
	case "search.accept":
		s.bar.Accept()
	case "search.close":
		s.bar.Close()
		s.filter()
	case "search.next":
		s.moveTo(s.sel+1, false)
	case "search.prev":
		s.moveTo(s.sel-1, false)
	case "filter.toggle":
		s.bar.Toggle()
		s.filter()
	case "sessions.select.down":
		s.moveTo(s.sel+1, true)
	case "sessions.select.up":
		s.moveTo(s.sel-1, true)
	case "sessions.resume":
		if ids := s.ids(); len(ids) > 0 {
			return func() tea.Msg { return OpenSessionMsg{Sessions: ids} }
		}
	case "sessions.fork":
		if ids := s.ids(); len(ids) > 0 {
			return func() tea.Msg { return ForkMsg{Sessions: ids} }
		}
	case "sessions.delete":
		s.confirm = s.ids()
	case "sessions.every":
		s.SetEvery(!s.every)
	case "list.down", "list.up", "list.top", "list.bottom", "list.page.down", "list.page.up":
		s.moveTo(listMove(id, s.sel, len(s.rows), page), false)
	default:
		if s.bar.Action(id) {
			s.filter()
		}
	}
	return nil
}

// Line along the bottom of the card, the pending question or the open filter bar with its count
func (s *Sessions) foot(w int) string {
	t := s.env.Theme
	if len(s.confirm) > 0 {
		what := s.confirm[0]
		if len(s.confirm) > 1 {
			what = render.Count(len(s.confirm), "session")
		}
		return render.Fit(" "+render.Bad("delete "+what+"?", t)+" "+render.Dim(s.env.keyed("confirm.yes", "yes")+" "+t.Glyphs.Sep+" "+s.env.keyed("confirm.no", "no"), t), w, t)
	}
	if !s.bar.Shown() {
		return ""
	}
	count := render.Count(len(s.rows), "session")
	if s.bar.Query() != "" {
		count = fmt.Sprintf("%d of %d", len(s.rows), len(s.all))
	}
	return s.bar.View(w, count, "filter", t)
}

// Line above the table naming the scope of the listing
func (s *Sessions) head(w int) string {
	t := s.env.Theme
	scope := "this project"
	if s.every {
		scope = "every project"
	}
	return render.Fit(" "+render.Dim(scope+" "+t.Glyphs.Sep+" "+s.env.keyed("sessions.every", "scope"), t), w, t)
}

func (s *Sessions) View(w, h int) string {
	s.w, s.h = w, h
	t := s.env.Theme
	var lines []string
	if s.err != nil {
		lines = append(lines, render.Truncate(render.Bad(s.err.Error(), t), w, t))
	}
	lines = append(lines, s.head(w))
	s.top = len(lines)
	foot := s.foot(w)
	room := h - len(lines)
	if foot != "" {
		room--
	}
	if len(s.rows) == 0 {
		empty := "no stored sessions"
		switch {
		case s.bar.Query() != "":
			empty = "no match"
		case !s.every && len(s.all) > 0:
			empty = "no sessions of this project"
		}
		return footed(append(lines, strings.Split(emptyView(w, room, empty, t), "\n")...), foot, h)
	}
	open := s.open()
	cols := []render.Column{{Title: ""}, {Title: "UPDATED"}, {Title: "TITLE", Flex: 1}}
	if s.every {
		cols = append(cols, render.Column{Title: "PROJECT", Max: 20})
	}
	cols = append(cols, render.Column{Title: "MODEL", Max: 24}, render.Column{Title: "MSGS", Right: true}, render.Column{Title: "TOKENS", Right: true})
	rows := make([][]string, 0, len(s.rows))
	for _, info := range s.rows {
		mark := " "
		switch {
		case open[info.ID]:
			mark = render.Cool(t.Glyphs.Live, t)
		case info.Busy:
			mark = render.Warm(t.Glyphs.Hollow, t)
		}
		title := info.Title
		if title == "" {
			title = info.ID
		}
		if info.Parent != "" {
			title += render.Dim(" "+t.Glyphs.Fork, t)
		}
		if info.Busy {
			title += render.Dim(" elsewhere", t)
		}
		if info.Error != "" {
			title = render.Bad(info.ID+" "+info.Error, t)
		}
		row := []string{mark, info.Updated.Local().Format("01-02 15:04"), title}
		if s.every {
			row = append(row, projectName(info.Labels[session.ProjectLabel]))
		}
		rows = append(rows, append(row, info.Model, fmt.Sprint(info.Messages), render.Tokens(info.Usage.Total())))
	}
	table := strings.Split(render.TableMarked(cols, rows, w, s.sel, s.marked, nil, t), "\n")
	table, s.start = window(table, s.sel, room)
	return footed(append(lines, table...), foot, h)
}

// Cursor in the filter bar along the bottom while it takes typing
func (s *Sessions) Cursor() *tea.Cursor {
	if !s.bar.Active() {
		return nil
	}
	return tea.NewCursor(s.bar.CursorX(), s.h-1)
}

func (s *Sessions) Status() string {
	if n := len(s.Selected()); n > 1 {
		return fmt.Sprintf("%d selected", n)
	}
	return ""
}

// Actions the listing answers in its current state
func (s *Sessions) Hints() []wm.Hint {
	switch {
	case len(s.confirm) > 0:
		return []wm.Hint{{Action: "confirm.yes", Label: "delete"}, {Action: "confirm.no", Label: "keep"}}
	case s.bar.Active():
		return []wm.Hint{{Action: "search.accept"}, {Action: "search.next", Pair: "search.prev", Label: "move"}, {Action: "search.close"}}
	}
	return []wm.Hint{{Action: "sessions.resume"}, {Action: "sessions.fork"}, {Action: "sessions.delete"}, {Action: "sessions.every"}, {Action: "filter.toggle"}, {Action: "list.down", Pair: "list.up", Label: "move"}, {Action: "sessions.select.down", Pair: "sessions.select.up", Label: "select"}}
}

// Base name of a project root, a dash for a session of no project
func projectName(root string) string {
	if root == "" {
		return "-"
	}
	return filepath.Base(root)
}
