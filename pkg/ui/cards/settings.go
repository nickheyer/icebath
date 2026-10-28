package cards

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Context window the compaction start is taken from when the catalog knows nothing of the model
const fallbackContext = 1000000

// App settings applied and saved the moment they change, each under its section
type settings struct {
	env     *Env
	sheet   *sheet
	editing bool
	before  string
	h       int
}

func newSettings(env *Env) *settings {
	tokens := func(v float64) string { return render.Tokens(int(v)) }
	var modes []string
	for _, m := range policy.Modes() {
		modes = append(modes, string(m))
	}
	fields := []*field{
		{key: "mode", label: "mode", group: "permissions", kind: choiceField, choices: modes, help: "default asks, acceptEdits allows file edits, plan only reads, bypassPermissions allows everything"},
		{key: "max_tokens", label: "max tokens", group: "model", kind: numberField, integer: true, optional: true, lo: 1, step: 1024, unit: tokens, help: "Output tokens per model turn, unset lets the model run to its limit"},
		{key: "max_rounds", label: "max rounds", group: "model", kind: numberField, integer: true, optional: true, lo: 1, step: 1, start: 50, help: "Tool rounds one prompt may take before the model must answer, unset for no limit"},
		{key: "temperature", label: "temperature", group: "model", kind: numberField, optional: true, lo: 0, step: 0.05, start: 1, help: "Sampling temperature, unset leaves it to the model"},
		{key: "compact_threshold", label: "compact threshold", group: "model", kind: numberField, integer: true, optional: true, lo: 1, step: 1000, unit: tokens, help: fmt.Sprintf("Context tokens at which old turns are summarized, unset compacts at %d%% of the model window, 0 never compacts", int(harness.DefaultCompactShare*100))},
		{key: "prune_after", label: "prune after", group: "model", kind: numberField, integer: true, optional: true, lo: 0, step: 1, start: harness.DefaultPruneAfter, help: fmt.Sprintf("Turns a tool result stays in context before a stub replaces it, unset prunes after %d, 0 never prunes", harness.DefaultPruneAfter)},
		{key: "theme", label: "theme", group: "interface", kind: choiceField, choices: theme.Names()},
		{key: "motion", label: "motion", group: "interface", kind: numberField, lo: 0, hi: 1, step: 0.05, start: 1, help: "Animation scale, 0 snaps and 1 springs"},
		{key: "cascade", label: "cascade", group: "interface", kind: choiceField, choices: []string{"true", "false"}, help: "Light sweep along the focused border"},
	}
	for _, sp := range harness.Settings() {
		fields = append(fields, extensionField(sp))
	}
	return &settings{env: env, sheet: newSheet(env, fields)}
}

// Field of a setting an extension registered, a choice, a slider or a text well under the extension's heading
func extensionField(sp harness.Setting) *field {
	f := &field{key: sp.Key, label: strings.ReplaceAll(sp.Key, "_", " "), group: or(sp.Source, "extensions"), help: sp.Help, optional: true, check: sp.Check}
	switch {
	case len(sp.Choices) > 0:
		f.kind, f.choices = choiceField, sp.Choices
	case sp.Number():
		f.kind, f.lo, f.hi, f.step, f.integer, f.start = numberField, sp.Lo, sp.Hi, sp.Step, sp.Integer, sp.Lo
	default:
		f.kind = textField
	}
	return f
}

func (s *settings) Title() string    { return "app" }
func (s *settings) Glyph() string    { return s.env.Theme.Glyphs.Settings }
func (s *settings) Key() string      { return "config:app" }
func (s *settings) Init() tea.Cmd    { return nil }
func (s *settings) Selectable() bool { return true }

// User config file the settings are written to
func (s *settings) Status() string {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return ""
	}
	return homed(filepath.Join(dir, harness.ConfigFile))
}

// Keymap contexts, the cell editor while a value is typed and the settings list otherwise
func (s *settings) Contexts() []string {
	if s.editing {
		return []string{keys.Edit}
	}
	return []string{keys.App, keys.List, keys.Panel}
}

// Values as the app stands and slider starts from the model on the endpoint, the typed one left alone
func (s *settings) sync() {
	values := map[string]string{}
	if s.env.Settings != nil {
		values = s.env.Settings()
	}
	if s.env.Mode != nil {
		values["mode"] = string(s.env.Mode.Mode())
	}
	output, window := 0, fallbackContext
	if s.env.Primary != nil {
		c := s.env.Primary().Catalog
		output = c.MaxOutput
		if c.Context > 0 {
			window = c.Context
		}
	}
	for i, f := range s.sheet.fields {
		switch f.key {
		case "max_tokens":
			f.start = float64(output)
			if output == 0 {
				f.start = f.step
			}
		case "compact_threshold":
			f.start = float64(window) * harness.DefaultCompactShare
		}
		if s.editing && i == s.sheet.at {
			continue
		}
		f.value = values[f.key]
		f.cursor = len([]rune(f.value))
	}
}

// Setting under the cursor
func (s *settings) current() *field {
	if f := s.sheet.focused(); f != nil {
		return f
	}
	return s.sheet.fields[0]
}

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

// Starts typing into a setting, keeping its value to restore on cancel
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
		if msg.Button == tea.MouseWheelUp {
			s.sheet.wheel(-1)
		} else {
			s.sheet.wheel(1)
		}
	}
	return s, nil
}

// Typed text goes into the value being typed, starts typing a number or text, or picks a choice
func (s *settings) typed(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	f := s.current()
	switch {
	case s.editing:
		f.typeText(text)
	case f.kind == numberField && strings.ContainsAny(text[:1], "0123456789.-"), f.kind == textField:
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
		s.sheet.at = listMove(id, s.sheet.at, len(s.sheet.fields), max(s.h/3, 1))
	case "config.app.left":
		return s.nudge(-1)
	case "config.app.right":
		return s.nudge(1)
	case "config.app.edit":
		switch {
		case f.toggle():
			f.flip()
			return s.apply(f)
		case f.kind == numberField || f.kind == textField:
			s.begin(f)
		}
	case "config.app.unset":
		if f.value != "" && f.unset() {
			return s.apply(f)
		}
	}
	return nil
}

// Saves a typed value, then selects the setting under the click and applies the choice, switch, slider or well hit
func (s *settings) click(x, y int) tea.Cmd {
	var done tea.Cmd
	if s.editing {
		if done = s.finish(true); s.editing {
			return done
		}
	}
	if s.sheet.page(x, y) {
		return done
	}
	i, h := s.sheet.hitAt(x, y)
	if i < 0 {
		return done
	}
	s.sheet.at = i
	if h == nil {
		return done
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

// Settings under their headings with the cursor row marked, the help of that row along the bottom
func (s *settings) View(w, h int) string {
	s.h = h
	s.sync()
	s.sheet.typing = s.editing
	return s.sheet.view(w, h, s.env.Theme)
}

// Cursor in the value being typed
func (s *settings) Cursor() *tea.Cursor { return s.sheet.cur }

// Actions the settings answer in their current state
func (s *settings) Hints() []wm.Hint {
	if s.editing {
		return []wm.Hint{{Action: "edit.apply"}, {Action: "edit.cancel"}}
	}
	f := s.current()
	out := []wm.Hint{{Action: "list.down", Pair: "list.up", Label: "move"}}
	switch {
	case f.toggle():
		out = append(out, wm.Hint{Action: "config.app.edit", Label: "toggle"})
	case f.kind == numberField || f.kind == textField:
		out = append(out, wm.Hint{Action: "config.app.left", Pair: "config.app.right", Label: "change"}, wm.Hint{Action: "config.app.edit"})
	default:
		out = append(out, wm.Hint{Action: "config.app.left", Pair: "config.app.right", Label: "change"})
	}
	if f.optional && f.value != "" {
		out = append(out, wm.Hint{Action: "config.app.unset"})
	}
	return out
}
