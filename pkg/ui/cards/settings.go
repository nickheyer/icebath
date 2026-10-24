package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Ceiling of the output and context sliders when the catalog knows nothing of the model
const (
	fallbackOutput  = 128000
	fallbackContext = 1000000
)

// App settings applied and saved the moment they change, choices shown whole and numbers on sliders
type settings struct {
	env     *Env
	fields  []*field
	sel     int
	editing bool
	before  string
	hits    []formHit
	tops    []int
	cur     *tea.Cursor
	w, h    int
}

func newSettings(env *Env) *settings {
	tokens := func(v float64) string { return render.Tokens(int(v)) }
	var modes []string
	for _, m := range mode.All() {
		modes = append(modes, string(m))
	}
	s := &settings{env: env}
	s.fields = []*field{
		{key: "mode", label: "mode", kind: choiceField, choices: modes, help: "default asks, acceptEdits allows file edits, plan only reads, bypassPermissions allows everything"},
		{key: "max_tokens", label: "max_tokens", kind: numberField, integer: true, optional: true, lo: 1024, step: 1024, start: harness.DefaultMaxTokens, unit: tokens, help: "Output tokens per model turn, unset sends " + render.Tokens(harness.DefaultMaxTokens)},
		{key: "max_rounds", label: "max_rounds", kind: numberField, integer: true, optional: true, lo: 1, hi: 500, step: 1, start: 50, help: "Model turns one prompt may take, unset for no limit"},
		{key: "temperature", label: "temperature", kind: numberField, optional: true, lo: 0, hi: 2, step: 0.05, start: 1, help: "Sampling temperature, unset leaves it to the model"},
		{key: "compact_threshold", label: "compact_threshold", kind: numberField, integer: true, optional: true, lo: 1000, step: 1000, unit: tokens, help: "Context tokens at which old turns are summarized, unset never compacts"},
		{key: "theme", label: "theme", kind: choiceField, choices: theme.Names(), help: "Color palette"},
		{key: "motion", label: "motion", kind: numberField, lo: 0, hi: 1, step: 0.05, start: 1, help: "Animation scale, 0 snaps and 1 springs"},
		{key: "cascade", label: "cascade", kind: choiceField, choices: []string{"true", "false"}, help: "Light sweep along the focused border"},
	}
	return s
}

func (s *settings) Title() string    { return "app" }
func (s *settings) Glyph() string    { return s.env.Theme.Glyphs.Settings }
func (s *settings) Key() string      { return "config:app" }
func (s *settings) Init() tea.Cmd    { return nil }
func (s *settings) Selectable() bool { return true }

// Keymap contexts, the cell editor while a number is typed and the settings list otherwise
func (s *settings) Contexts() []string {
	if s.editing {
		return []string{keys.Edit}
	}
	return []string{keys.App, keys.List, keys.Panel}
}

// Values and ranges as the app and the model on the endpoint stand, the one being typed left alone
func (s *settings) sync() {
	values := map[string]string{}
	if s.env.Settings != nil {
		values = s.env.Settings()
	}
	if s.env.Mode != nil {
		values["mode"] = string(s.env.Mode.Mode())
	}
	output, window := fallbackOutput, fallbackContext
	if s.env.Primary != nil {
		if c := s.env.Primary().Catalog; c.MaxOutput > 0 {
			output = c.MaxOutput
		}
		if c := s.env.Primary().Catalog; c.Context > 0 {
			window = c.Context
		}
	}
	for i, f := range s.fields {
		switch f.key {
		case "max_tokens":
			f.hi = float64(output)
		case "compact_threshold":
			f.hi = float64(window)
			f.start = float64(window * 8 / 10)
		}
		if s.editing && i == s.sel {
			continue
		}
		f.value = values[f.key]
		f.cursor = len([]rune(f.value))
	}
}

// Setting under the cursor
func (s *settings) current() *field { return s.fields[min(max(s.sel, 0), len(s.fields)-1)] }

// Saves the value of a setting quietly, the row itself showing the change
func (s *settings) apply(f *field) tea.Cmd {
	key, value := f.key, strings.TrimSpace(f.value)
	return func() tea.Msg { return SettingMsg{Key: key, Value: value, Quiet: true} }
}

// Steps the setting under the cursor and saves it
func (s *settings) nudge(dir int) tea.Cmd {
	f := s.current()
	before := f.value
	f.nudge(dir)
	if f.value == before {
		return nil
	}
	return s.apply(f)
}

// Starts typing into a number setting, keeping its value to restore on cancel
func (s *settings) begin(f *field) {
	s.editing, s.before = true, f.value
	f.cursor = len([]rune(f.value))
}

// Ends typing, saving the text when apply holds and it passes the checks
func (s *settings) finish(apply bool) tea.Cmd {
	f := s.current()
	if !apply {
		f.value, s.editing = s.before, false
		return nil
	}
	if p := f.problem(); p != "" {
		return wm.Notify(p, wm.Warn)
	}
	s.editing = false
	if f.value == s.before {
		return nil
	}
	return s.apply(f)
}

func (s *settings) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.FocusMsg:
		if !msg.Focused && s.editing {
			s.finish(false)
		}
	case wm.ActionMsg:
		return s, s.act(msg.ID)
	case tea.KeyPressMsg:
		return s, s.typed(msg.Text)
	case tea.PasteMsg:
		return s, s.typed(msg.Content)
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return s, s.click(msg.X, msg.Y)
		}
	case tea.MouseWheelMsg:
		if s.editing {
			return s, nil
		}
		if msg.Button == tea.MouseWheelUp {
			s.sel = max(s.sel-1, 0)
		} else {
			s.sel = min(s.sel+1, len(s.fields)-1)
		}
	}
	return s, nil
}

// Typed text goes into the number being typed, starts typing a number, or jumps to a choice
func (s *settings) typed(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	f := s.current()
	switch {
	case s.editing:
		f.typeText(text)
	case f.kind == numberField && strings.ContainsAny(text[:1], "0123456789.-"):
		s.begin(f)
		f.value, f.cursor = "", 0
		f.typeText(text)
	case f.kind == choiceField:
		before := f.value
		f.typeText(text)
		if f.value != before {
			return s.apply(f)
		}
	}
	return nil
}

// Carries out a settings action on the cursor, the value under it or the text being typed
func (s *settings) act(id string) tea.Cmd {
	f := s.current()
	if s.editing {
		switch id {
		case "edit.apply":
			return s.finish(true)
		case "edit.cancel":
			return s.finish(false)
		case "edit.delete.back":
			f.edit("delete.back")
		case "edit.clear":
			f.edit("clear")
		case "edit.end":
			f.edit("end")
		}
		return nil
	}
	switch id {
	case "list.down", "list.up", "list.top", "list.bottom", "list.page.down", "list.page.up":
		s.sel = listMove(id, s.sel, len(s.fields), max(s.h-2, 1))
	case "config.app.left":
		return s.nudge(-1)
	case "config.app.right":
		return s.nudge(1)
	case "config.app.edit":
		if f.kind == numberField {
			s.begin(f)
		}
	case "config.app.unset":
		if f.value != "" && f.unset() {
			return s.apply(f)
		}
	}
	return nil
}

// Saves a typed number, then selects the setting under the click and applies the choice, slider or box hit
func (s *settings) click(x, y int) tea.Cmd {
	var done tea.Cmd
	if s.editing {
		if done = s.finish(true); s.editing {
			return done
		}
	}
	for _, h := range s.hits {
		if y != h.y || x < h.x0 || x >= h.x1 {
			continue
		}
		for i, f := range s.fields {
			if f == h.field {
				s.sel = i
			}
		}
		f := h.field
		if h.hit.choice == hitBox {
			s.begin(f)
			return done
		}
		before := f.value
		f.click(h.hit, x-h.x0+h.hit.x0)
		if f.value != before {
			return tea.Batch(done, s.apply(f))
		}
		return done
	}
	for i, top := range s.tops {
		if y >= top && (i+1 == len(s.tops) || y < s.tops[i+1]) {
			s.sel = i
		}
	}
	return done
}

// Settings spaced out above room for the longest help, the help of the cursor along the bottom
func (s *settings) View(w, h int) string {
	s.w, s.h = w, h
	s.sync()
	t := s.env.Theme
	hw := max(w-3, 1)
	room := 0
	for _, f := range s.fields {
		room = max(room, strings.Count(render.Wrap(f.help, hw), "\n")+1)
	}
	lines := s.rows(w, h-room-1)
	help := strings.Split(render.Wrap(s.current().help, hw), "\n")
	for len(help) < room {
		help = append(help, "")
	}
	for len(lines) < h-room-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:max(min(len(lines), h-room-1), 0)], "")
	for _, line := range help {
		lines = append(lines, "   "+render.Dim(line, t))
	}
	return strings.Join(lines[:min(len(lines), h)], "\n")
}

// Setting rows spaced within height h, the cursor row marked, recording clicks, tops and the cursor
func (s *settings) rows(w, h int) []string {
	t := s.env.Theme
	lw := 0
	for _, f := range s.fields {
		lw = max(lw, ansi.StringWidth(f.label))
	}
	lw += 5
	vw := max(w-lw, 8)
	cols := sliderColumns(s.fields)
	lookOf := func(i int, framed bool) look {
		return look{focused: i == s.sel, typing: i == s.sel && s.editing, framed: framed}
	}
	painted := make([][]string, len(s.fields))
	marks := make([][]hit, len(s.fields))
	blocks := make([]stack, len(s.fields))
	used := 0
	s.cur = nil
	for i, f := range s.fields {
		rows, hits, cx := f.render(vw, lookOf(i, true), cols, t)
		painted[i], marks[i] = rows, hits
		blocks[i] = stack{rows: len(rows), well: f.welled()}
		used += len(rows)
		if i == s.sel && cx >= 0 {
			s.cur = tea.NewCursor(lw+cx, 0)
		}
	}
	gaps := spaceOut(blocks, h-used)
	if touching(blocks, gaps) {
		for i, f := range s.fields {
			if f.welled() && i != s.sel {
				painted[i], _, _ = f.render(vw, lookOf(i, false), cols, t)
			}
		}
	}
	s.hits, s.tops = s.hits[:0], s.tops[:0]
	var lines []string
	for i, f := range s.fields {
		if gaps[i] {
			lines = append(lines, "")
		}
		gutter, st := "   ", lipgloss.NewStyle().Foreground(t.TextDim)
		if i == s.sel {
			gutter = lipgloss.NewStyle().Foreground(t.Ice).Render(" " + t.Glyphs.Cursor + " ")
			st = lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
		}
		top := len(lines)
		s.tops = append(s.tops, top)
		for r, row := range painted[i] {
			label := ""
			if r == 0 {
				label = gutter + st.Render(f.label)
			}
			lines = append(lines, render.Fit(label, lw, t)+row)
		}
		for _, hh := range marks[i] {
			s.hits = append(s.hits, formHit{y: top + hh.row, x0: lw + hh.x0, x1: lw + hh.x1, field: f, hit: hh})
		}
		if i == s.sel && s.cur != nil {
			s.cur.Y = top
		}
	}
	return lines
}

// Cursor in the number being typed
func (s *settings) Cursor() *tea.Cursor { return s.cur }

// Actions the settings answer in their current state
func (s *settings) Hints() []wm.Hint {
	if s.editing {
		return []wm.Hint{{Action: "edit.apply"}, {Action: "edit.cancel"}}
	}
	f := s.current()
	out := []wm.Hint{{Action: "list.down", Pair: "list.up", Label: "move"}, {Action: "config.app.left", Pair: "config.app.right", Label: "change"}}
	if f.kind == numberField {
		out = append(out, wm.Hint{Action: "config.app.edit"})
	}
	if f.optional && f.value != "" {
		out = append(out, wm.Hint{Action: "config.app.unset"})
	}
	return out
}
