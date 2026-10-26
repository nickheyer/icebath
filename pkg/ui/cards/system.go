package cards

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Settings the editor switches, in the order of their rows
const (
	settingContext = iota
	settingBare
	settingGhost
	settingCount
)

// Rows the view keeps beside the prompt, its header and the blank row before the settings
const systemChrome = 2

// Columns the setting names take
const settingLabel = 9

// Choices every setting row offers
var settingChoices = []string{"on", "off"}

// Floating editor of one session's system prompt with switches for its context, bare and ghost
type System struct {
	env    *Env
	live   *harness.Live
	editor textarea.Model
	// Base prompt the session sends, the editor holding unsaved changes while its text differs
	applied string
	// Control holding the keys, zero for the prompt and one past the index of a setting
	focus   int
	focused bool
	rows    int
	hits    []systemHit
}

// Chip of one setting as the view last drew it
type systemHit struct {
	y, x0, x1 int
	setting   int
	on        bool
}

// Builds the editor over the session, loaded with the base prompt it sends
func NewSystem(env *Env, live *harness.Live) *System {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetVirtualCursor(false)
	ta.KeyMap = editingKeys(env.Keys)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("enter", "ctrl+j"))
	ta.SetStyles(editorStyles(env.Theme))
	s := &System{env: env, live: live, editor: ta}
	s.load()
	return s
}

func (s *System) Title() string       { return "SYSTEM" }
func (s *System) Glyph() string       { return s.env.Theme.Glyphs.Settings }
func (s *System) Key() string         { return "system:" + s.live.Session.ID() }
func (s *System) Init() tea.Cmd       { return nil }
func (s *System) Floating() bool      { return true }
func (s *System) MinSize() (int, int) { return 72, 16 }
func (s *System) Contexts() []string  { return []string{keys.System} }

// Base prompt the session sends right now, its own or the default
func (s *System) base() string {
	if t := harness.TuningOf(s.live.Session); t.Custom {
		return t.System
	}
	return s.env.DefaultBase()
}

// Fills the editor with the base the session sends, dropping unsaved text
func (s *System) load() {
	s.applied = s.base()
	s.editor.SetValue(s.applied)
}

// Reports whether the editor holds text the session does not send yet
func (s *System) edited() bool { return s.editor.Value() != s.applied }

// Marks unsaved text beside the title
func (s *System) Status() string {
	if s.edited() {
		return "edited"
	}
	return ""
}

// Asks the app to apply the tuning to the session
func (s *System) tune(t harness.Tuning) tea.Cmd {
	id := s.live.Session.ID()
	return func() tea.Msg { return TuneMsg{Session: id, Tuning: t} }
}

// Sends the edited text from the next turn on, text matching the default leaving the session on the default
func (s *System) save() tea.Cmd {
	t := harness.TuningOf(s.live.Session)
	text := s.editor.Value()
	t.System, t.Custom = text, text != s.env.DefaultBase()
	if !t.Custom {
		t.System = ""
	}
	s.applied = text
	return s.tune(t)
}

// Puts the default prompt back in the editor and the session
func (s *System) reset() tea.Cmd {
	t := harness.TuningOf(s.live.Session)
	t.System, t.Custom = "", false
	s.applied = s.env.DefaultBase()
	s.editor.SetValue(s.applied)
	return s.tune(t)
}

// Empties the editor, the session keeping its prompt until the text is saved
func (s *System) clear() tea.Cmd {
	s.editor.SetValue("")
	return nil
}

// Whether a setting is on in the session tuning
func (s *System) setting(i int) bool {
	t := harness.TuningOf(s.live.Session)
	switch i {
	case settingContext:
		return !t.NoContext
	case settingBare:
		return t.Bare
	}
	return t.Ghost
}

// Turns a setting on or off in the session tuning
func (s *System) set(i int, on bool) tea.Cmd {
	t := harness.TuningOf(s.live.Session)
	switch i {
	case settingContext:
		t.NoContext = !on
	case settingBare:
		t.Bare = on
	case settingGhost:
		t.Ghost = on
	}
	return s.tune(t)
}

// Moves the keys to another control, the prompt taking the cursor
func (s *System) move(d int) tea.Cmd {
	s.focus = ((s.focus+d)%(settingCount+1) + settingCount + 1) % (settingCount + 1)
	return s.aim()
}

// Points the editor focus at the control holding the keys
func (s *System) aim() tea.Cmd {
	if s.focused && s.focus == 0 {
		return s.editor.Focus()
	}
	s.editor.Blur()
	return nil
}

func (s *System) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.FocusMsg:
		s.focused = msg.Focused
		if s.focused && !s.edited() {
			s.load()
		}
		return s, s.aim()
	case wm.ActionMsg:
		return s, s.act(msg)
	case tea.PasteMsg:
		s.focus = 0
		s.aim()
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return s, cmd
	case tea.KeyPressMsg:
		if s.focus > 0 {
			return s, s.press(msg)
		}
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return s, cmd
	case tea.MouseClickMsg:
		return s, s.click(msg)
	}
	return s, nil
}

// Carries out a keymap action, a space reaching the prompt as text while it holds the keys
func (s *System) act(msg wm.ActionMsg) tea.Cmd {
	switch msg.ID {
	case "system.save":
		return s.save()
	case "system.reset":
		return s.reset()
	case "system.clear":
		return s.clear()
	case "system.next":
		return s.move(1)
	case "system.prev":
		return s.move(-1)
	case "system.close":
		return wm.RequestClose(s)
	case "system.toggle":
		if s.focus == 0 {
			var cmd tea.Cmd
			s.editor, cmd = s.editor.Update(msg.Key)
			return cmd
		}
		return s.set(s.focus-1, !s.setting(s.focus-1))
	}
	return nil
}

// Answers keys on a focused setting, left and right picking a chip and enter switching it
func (s *System) press(msg tea.KeyPressMsg) tea.Cmd {
	i := s.focus - 1
	switch msg.String() {
	case "left":
		return s.set(i, true)
	case "right":
		return s.set(i, false)
	case "enter":
		return s.set(i, !s.setting(i))
	}
	return nil
}

// Focuses the prompt under a click or switches the setting whose chip it lands on
func (s *System) click(msg tea.MouseClickMsg) tea.Cmd {
	if msg.Button != tea.MouseLeft {
		return nil
	}
	for _, h := range s.hits {
		if msg.Y == h.y && msg.X >= h.x0 && msg.X < h.x1 {
			s.focus = h.setting + 1
			s.aim()
			return s.set(h.setting, h.on)
		}
	}
	if msg.Y >= 1 && msg.Y < 1+s.rows {
		s.focus = 0
		return s.aim()
	}
	return nil
}

// Cursor of the prompt below its header and beside its gutter
func (s *System) Cursor() *tea.Cursor {
	if !s.focused || s.focus != 0 {
		return nil
	}
	cur := s.editor.Cursor()
	if cur == nil {
		return nil
	}
	cur.X += 2
	cur.Y++
	return cur
}

// Headers and body fields the profile sends for a directive, nothing when it names none
func sends(name string, d *provider.Directive) string {
	var keys []string
	if d != nil {
		for k := range d.Header {
			keys = append(keys, k)
		}
		for k := range d.Extra {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return name + " sends nothing"
	}
	slices.Sort(keys)
	return name + " sends " + strings.Join(keys, ", ")
}

// Words beside a setting saying what it does for this session and endpoint
func (s *System) explain(i int) string {
	ep := s.live.Endpoint
	switch i {
	case settingContext:
		env := s.env.Environment(ep.Model)
		return "environment block, " + render.Count(strings.Count(env, "\n")+1, "line")
	case settingBare:
		return sends(ep.Profile.Name, ep.Profile.Bare)
	}
	return "nothing on disk, " + sends(ep.Profile.Name, ep.Profile.Ghost)
}

// Header naming the prompt as the default, the session's own or empty, and marking unsaved text
func (s *System) header(w int) string {
	t := s.env.Theme
	dim := lipgloss.NewStyle().Foreground(t.TextDim)
	kind := dim.Render("default")
	switch tu := harness.TuningOf(s.live.Session); {
	case tu.Custom && tu.System == "":
		kind = lipgloss.NewStyle().Foreground(t.Accent).Render("empty")
	case tu.Custom:
		kind = lipgloss.NewStyle().Foreground(t.Ice).Render("custom")
	}
	if s.edited() {
		kind += dim.Render(" "+t.Glyphs.Sep+" ") + lipgloss.NewStyle().Foreground(t.Accent).Render("edited")
	}
	left := dim.Render("prompt")
	return left + strings.Repeat(" ", max(w-ansi.StringWidth(left)-ansi.StringWidth(kind), 1)) + kind
}

// One setting row, its name, its on and off chips and what it does, recording the chips for clicks
func (s *System) row(i, y, w int) string {
	t := s.env.Theme
	names := [settingCount]string{"context", "bare", "ghost"}
	focused := s.focused && s.focus == i+1
	nameFg := t.TextDim
	if focused {
		nameFg = t.Text
	}
	picked := "off"
	if s.setting(i) {
		picked = "on"
	}
	chipRows, hits := chips(settingChoices, picked, focused, w, t)
	x := settingLabel
	for _, h := range hits {
		s.hits = append(s.hits, systemHit{y: y, x0: x + h.x0, x1: x + h.x1, setting: i, on: settingChoices[h.choice] == "on"})
	}
	line := lipgloss.NewStyle().Foreground(nameFg).Bold(focused).Render(render.Fit(names[i], settingLabel, t)) + chipRows[0]
	used := ansi.StringWidth(line)
	if room := w - used - 2; room > 0 {
		line += "  " + lipgloss.NewStyle().Foreground(t.TextDim).Render(render.Truncate(s.explain(i), room, t))
	}
	return line
}

func (s *System) View(w, h int) string {
	t := s.env.Theme
	s.rows = max(h-systemChrome-settingCount, 1)
	s.editor.SetWidth(max(w-2, 1))
	s.editor.SetHeight(s.rows)
	gutter := lipgloss.NewStyle().Foreground(t.Steel).Render(t.Glyphs.Bar + " ")
	if s.focused && s.focus == 0 {
		gutter = lipgloss.NewStyle().Foreground(t.Ice).Render(t.Glyphs.Bar + " ")
	}
	lines := []string{s.header(w)}
	body := strings.Split(s.editor.View(), "\n")
	for i := range s.rows {
		text := ""
		if i < len(body) {
			text = body[i]
		}
		lines = append(lines, gutter+render.Fit(text, max(w-2, 1), t))
	}
	lines = append(lines, "")
	s.hits = s.hits[:0]
	for i := range settingCount {
		lines = append(lines, s.row(i, len(lines), w))
	}
	return strings.Join(lines, "\n")
}

// Keys worth a reminder, the switch only while a setting holds the keys
func (s *System) Hints() []wm.Hint {
	if s.focus > 0 {
		return []wm.Hint{{Action: "system.toggle"}, {Action: "system.next", Pair: "system.prev", Label: "move"}, {Action: "system.close"}}
	}
	return []wm.Hint{{Action: "system.save"}, {Action: "system.clear"}, {Action: "system.reset"}, {Action: "system.next", Pair: "system.prev", Label: "move"}, {Action: "system.close"}}
}
