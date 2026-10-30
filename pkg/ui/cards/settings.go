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

// Step of the max tokens slider, and its start when the catalog knows no output limit
const tokenStep = 1024

// Kind of value a settings row takes
type FieldKind int

// Text, masked text, a choice or switch, a number, or multi line text
const (
	FieldText   = FieldKind(textField)
	FieldSecret = FieldKind(secretField)
	FieldChoice = FieldKind(choiceField)
	FieldNumber = FieldKind(numberField)
	FieldArea   = FieldKind(areaField)
)

// One row of a settings sheet, its value held as text and empty while unset
type Field struct {
	Key, Label, Group, Help, Placeholder string
	Kind                                 FieldKind
	Choices                              []string
	// Row may be left empty, an empty value returning the setting to its default
	Optional bool
	Integer  bool
	// Floor, ceiling, step and unset start of a number, no ceiling when it sits at or under the floor
	Lo, Hi, Step, Start float64
	// Labels the range ends of a number with a ceiling
	Unit func(float64) string
	// Refuses a value with the reason
	Check func(string) error
}

// What a settings card edits and where it writes
type SettingsSpec struct {
	Title, Key, Glyph string
	// Trailer along the bottom edge, such as the file the settings live in
	Status func() string
	// Keymap contexts while no value is typed, the app tab contexts when empty
	Contexts []string
	Fields   []Field
	// Current value of every key, empty text while unset
	Values func() map[string]string
	// Slider start per key worked out at view time, nil for none
	Starts func() map[string]float64
	// Choices per key worked out at view time, nil for none
	Choices func() map[string][]string
	// Saves one value, an empty one unsetting it
	Apply func(key, value string) tea.Cmd
	// Runs as the card opens, nil for nothing
	Init func() tea.Cmd
}

// Card of rows applied and saved the moment they change, each under its section
type Settings struct {
	env     *Env
	spec    SettingsSpec
	sheet   *sheet
	editing bool
	before  string
	h       int
}

// Card over the rows of the spec, reading and writing the values through it
func NewSettings(env *Env, spec SettingsSpec) *Settings {
	fields := make([]*field, 0, len(spec.Fields))
	for _, f := range spec.Fields {
		fields = append(fields, &field{key: f.Key, label: f.Label, group: f.Group, help: f.Help, placeholder: f.Placeholder, kind: fieldKind(f.Kind), choices: f.Choices, optional: f.Optional, integer: f.Integer, lo: f.Lo, hi: f.Hi, step: f.Step, start: f.Start, unit: f.Unit, check: f.Check})
	}
	return &Settings{env: env, spec: spec, sheet: newSheet(env, fields)}
}

// App tab rows under their sections, each saved through a setting message
func newSettings(env *Env) *Settings {
	tokens := func(v float64) string { return render.Tokens(int(v)) }
	var modes []string
	for _, m := range policy.Modes() {
		modes = append(modes, string(m))
	}
	fields := []Field{
		{Key: "mode", Label: "mode", Group: "permissions", Kind: FieldChoice, Choices: modes, Help: "default asks, acceptEdits allows file edits, plan only reads, bypassPermissions allows everything"},
		{Key: "max_tokens", Label: "max tokens", Group: "model", Kind: FieldNumber, Integer: true, Optional: true, Lo: 1, Step: tokenStep, Unit: tokens, Help: "Output tokens per model turn, unset lets the model run to its limit"},
		{Key: "max_rounds", Label: "max rounds", Group: "model", Kind: FieldNumber, Integer: true, Optional: true, Lo: 1, Step: 1, Start: 50, Help: "Tool rounds one prompt may take before the model must answer, unset for no limit"},
		{Key: "temperature", Label: "temperature", Group: "model", Kind: FieldNumber, Optional: true, Lo: 0, Step: 0.05, Start: 1, Help: "Sampling temperature, unset leaves it to the model"},
		{Key: "compact_threshold", Label: "compact threshold", Group: "model", Kind: FieldNumber, Integer: true, Optional: true, Lo: 1, Step: 1000, Unit: tokens, Help: fmt.Sprintf("Context tokens at which old turns are summarized, unset compacts at %d%% of the model window, 0 never compacts", int(harness.DefaultCompactShare*100))},
		{Key: "prune_after", Label: "prune after", Group: "model", Kind: FieldNumber, Integer: true, Optional: true, Lo: 0, Step: 1, Start: harness.DefaultPruneAfter, Help: fmt.Sprintf("Turns a tool result stays in context before a stub replaces it, unset prunes after %d, 0 never prunes", harness.DefaultPruneAfter)},
		{Key: "theme", Label: "theme", Group: "interface", Kind: FieldChoice, Choices: theme.Names()},
		{Key: "motion", Label: "motion", Group: "interface", Kind: FieldNumber, Lo: 0, Hi: 1, Step: 0.05, Start: 1, Help: "Animation scale, 0 snaps and 1 springs"},
		{Key: "cascade", Label: "cascade", Group: "interface", Kind: FieldChoice, Choices: []string{"true", "false"}, Help: "Light sweep along the focused border"},
		{Key: "terminal_shell", Label: "shell", Group: "terminal", Kind: FieldText, Optional: true, Placeholder: "$SHELL", Help: "Command line a terminal window runs, the login shell when empty"},
		{Key: "terminal_scrollback", Label: "scrollback", Group: "terminal", Kind: FieldNumber, Integer: true, Optional: true, Lo: 0, Step: 1000, Start: harness.DefaultScrollback, Help: fmt.Sprintf("Rows a terminal window keeps above its screen, unset keeps %d", harness.DefaultScrollback)},
	}
	for _, sp := range harness.Settings() {
		fields = append(fields, extensionField(sp))
	}
	return NewSettings(env, SettingsSpec{
		Title:  "app",
		Key:    "config:app",
		Glyph:  env.Theme.Glyphs.Settings,
		Status: appConfigFile,
		Fields: fields,
		Values: func() map[string]string {
			values := map[string]string{}
			if env.Settings != nil {
				values = env.Settings()
			}
			if env.Mode != nil {
				values["mode"] = string(env.Mode.Mode())
			}
			return values
		},
		Starts: func() map[string]float64 {
			output, window := 0, fallbackContext
			if env.Primary != nil {
				c := env.Primary().Catalog
				output = c.MaxOutput
				if c.Context > 0 {
					window = c.Context
				}
			}
			start := float64(output)
			if output == 0 {
				start = tokenStep
			}
			return map[string]float64{"max_tokens": start, "compact_threshold": float64(window) * harness.DefaultCompactShare}
		},
		Apply: func(key, value string) tea.Cmd {
			return func() tea.Msg { return SettingMsg{Key: key, Value: value, Quiet: true} }
		},
	})
}

// User config file the app settings are written to
func appConfigFile() string {
	dir, err := profiles.ConfigDir()
	if err != nil {
		return ""
	}
	return homed(filepath.Join(dir, harness.ConfigFile))
}

// Row of a setting an extension registered, masked, a choice, a slider or a text well under its heading
func extensionField(sp harness.Setting) Field {
	f := Field{Key: sp.Key, Label: strings.ReplaceAll(sp.Key, "_", " "), Group: or(sp.Source, "extensions"), Help: sp.Help, Optional: true, Check: sp.Check}
	switch {
	case sp.Secret:
		f.Kind = FieldSecret
	case len(sp.Choices) > 0:
		f.Kind, f.Choices = FieldChoice, sp.Choices
	case sp.Number():
		f.Kind, f.Lo, f.Hi, f.Step, f.Integer, f.Start = FieldNumber, sp.Lo, sp.Hi, sp.Step, sp.Integer, sp.Lo
	default:
		f.Kind = FieldText
	}
	return f
}

func (s *Settings) Title() string    { return s.spec.Title }
func (s *Settings) Glyph() string    { return s.spec.Glyph }
func (s *Settings) Key() string      { return s.spec.Key }
func (s *Settings) Selectable() bool { return true }

// Command of the spec run as the card opens
func (s *Settings) Init() tea.Cmd {
	if s.spec.Init == nil {
		return nil
	}
	return s.spec.Init()
}

// Trailer the spec gives, such as the file the settings are written to
func (s *Settings) Status() string {
	if s.spec.Status == nil {
		return ""
	}
	return s.spec.Status()
}

// Editor keys while a value is typed, multi line ones for a text area, the list keys otherwise
func (s *Settings) Contexts() []string {
	switch {
	case s.editing && s.focusedKind() == areaField:
		return []string{keys.Area, keys.Form}
	case s.editing:
		return []string{keys.Edit}
	case len(s.spec.Contexts) > 0:
		return s.spec.Contexts
	}
	return []string{keys.App, keys.List, keys.Panel}
}

// Values, slider starts and choices as the spec gives them now, the typed one left alone
func (s *Settings) sync() {
	values := s.spec.Values()
	var starts map[string]float64
	if s.spec.Starts != nil {
		starts = s.spec.Starts()
	}
	var choices map[string][]string
	if s.spec.Choices != nil {
		choices = s.spec.Choices()
	}
	for i, f := range s.sheet.fields {
		if v, ok := starts[f.key]; ok {
			f.start = v
		}
		if c, ok := choices[f.key]; ok {
			f.choices = c
		}
		if s.editing && i == s.sheet.at {
			continue
		}
		f.value = values[f.key]
		f.cursor = len([]rune(f.value))
	}
}

// What the row under the cursor takes, text when the sheet has no rows
func (s *Settings) focusedKind() fieldKind {
	if f := s.sheet.focused(); f != nil {
		return f.kind
	}
	return textField
}

// Reports whether the row takes typed text rather than picks
func typable(f *field) bool {
	switch f.kind {
	case textField, secretField, numberField, areaField:
		return true
	}
	return false
}

// Saves the value of a setting through the spec, the row itself showing the change
func (s *Settings) apply(f *field) tea.Cmd {
	return s.spec.Apply(f.key, strings.TrimSpace(f.value))
}

// Steps the setting under the cursor and saves it
func (s *Settings) nudge(f *field, dir int) tea.Cmd {
	before := f.value
	f.nudge(dir)
	if f.value == before {
		return nil
	}
	return s.apply(f)
}

// Starts typing into a setting, keeping its value to restore on cancel
func (s *Settings) begin(f *field) {
	s.editing, s.before = true, f.value
	f.cursor = len([]rune(f.value))
	if f.kind == areaField {
		s.sheet.prepare()
		s.sheet.aim()
		if f.area.Value() != f.value {
			f.area.SetValue(f.value)
		}
		f.area.MoveToEnd()
	}
}

// Ends typing, saving the text when apply holds and it passes the checks
func (s *Settings) finish(apply bool) tea.Cmd {
	f := s.sheet.focused()
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

// Saves the value being typed and moves the cursor to the neighbouring row in the direction
func (s *Settings) leave(dir int) tea.Cmd {
	cmd := s.finish(true)
	if !s.editing {
		s.sheet.at = min(max(s.sheet.at+dir, 0), len(s.sheet.fields)-1)
	}
	return cmd
}

func (s *Settings) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.FocusMsg:
		if !msg.Focused && s.editing {
			s.finish(false)
		}
	case wm.ActionMsg:
		return s, s.act(msg.ID)
	case tea.KeyPressMsg:
		if s.focusedKind() == areaField {
			return s, s.keyArea(msg, msg.Text)
		}
		return s, s.typed(msg.Text)
	case tea.PasteMsg:
		if s.focusedKind() == areaField {
			return s, s.keyArea(msg, msg.Content)
		}
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
func (s *Settings) typed(text string) tea.Cmd {
	f := s.sheet.focused()
	if text == "" || f == nil {
		return nil
	}
	switch {
	case s.editing:
		f.typeText(text)
	case f.kind == numberField && strings.ContainsAny(text[:1], "0123456789.-"), f.kind == textField, f.kind == secretField:
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

// Sends a key or paste to the multi line row, starting to type at its end when idle
func (s *Settings) keyArea(msg tea.Msg, text string) tea.Cmd {
	f := s.sheet.focused()
	if !s.editing {
		if text == "" {
			return nil
		}
		s.begin(f)
	}
	return s.typeArea(f, msg)
}

// Feeds a message to the editor of the multi line row and takes its text back as the value
func (s *Settings) typeArea(f *field, msg tea.Msg) tea.Cmd {
	s.sheet.prepare()
	s.sheet.aim()
	var cmd tea.Cmd
	*f.area, cmd = f.area.Update(msg)
	f.value = f.area.Value()
	f.cursor = len([]rune(f.value))
	return cmd
}

// Carries out a settings action on the cursor, the value under it or the text being typed
func (s *Settings) act(id string) tea.Cmd {
	f := s.sheet.focused()
	if f == nil {
		return nil
	}
	if s.editing && f.kind == areaField {
		return s.actArea(f, id)
	}
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
		return s.nudge(f, -1)
	case "config.app.right":
		return s.nudge(f, 1)
	case "config.app.edit":
		switch {
		case f.toggle():
			f.flip()
			return s.apply(f)
		case typable(f):
			s.begin(f)
		}
	case "config.app.unset":
		if f.value != "" && f.unset() {
			return s.apply(f)
		}
	}
	return nil
}

// Carries out a dialog or editor action on the multi line row being typed
func (s *Settings) actArea(f *field, id string) tea.Cmd {
	a := f.area
	if alias, ok := areaEdits[id]; ok {
		if press := keys.Press(s.env.Keys.First(alias)); press.Code != 0 {
			return s.typeArea(f, press)
		}
		return nil
	}
	switch id {
	case "area.save", "form.submit":
		return s.finish(true)
	case "form.cancel":
		return s.finish(false)
	case "form.next":
		return s.leave(1)
	case "form.prev":
		return s.leave(-1)
	case "area.newline":
		a.InsertString("\n")
	case "area.up":
		if a.Line() == 0 && a.LineInfo().RowOffset == 0 {
			return s.leave(-1)
		}
		a.CursorUp()
	case "area.down":
		if li := a.LineInfo(); a.Line() == a.LineCount()-1 && li.RowOffset >= li.Height-1 {
			return s.leave(1)
		}
		a.CursorDown()
	case "form.clear":
		a.SetValue("")
	case "form.unset":
		if !f.unset() {
			return nil
		}
		a.SetValue("")
	default:
		return nil
	}
	f.value = a.Value()
	f.cursor = len([]rune(f.value))
	return nil
}

// Saves a typed value, then selects the setting under the click and applies the choice, switch, slider or well hit
func (s *Settings) click(x, y int) tea.Cmd {
	var done tea.Cmd
	if s.editing {
		if i, h := s.sheet.hitAt(x, y); h != nil && i == s.sheet.at && h.field.kind == areaField && h.hit.choice == hitBox {
			h.field.click(h.hit, x-h.x0+h.hit.x0)
			return nil
		}
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
		if f.kind == areaField {
			f.click(h.hit, x-h.x0+h.hit.x0)
		}
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
func (s *Settings) View(w, h int) string {
	s.h = h
	s.sync()
	s.sheet.typing = s.editing
	return s.sheet.view(w, h, s.env.Theme)
}

// Cursor in the value being typed
func (s *Settings) Cursor() *tea.Cursor { return s.sheet.cur }

// Actions the settings answer in their current state
func (s *Settings) Hints() []wm.Hint {
	f := s.sheet.focused()
	switch {
	case s.editing && f.kind == areaField:
		return []wm.Hint{{Action: "area.save"}, {Action: "form.cancel"}, {Action: "area.newline"}}
	case s.editing:
		return []wm.Hint{{Action: "edit.apply"}, {Action: "edit.cancel"}}
	case f == nil:
		return nil
	}
	out := []wm.Hint{{Action: "list.down", Pair: "list.up", Label: "move"}}
	switch {
	case f.toggle():
		out = append(out, wm.Hint{Action: "config.app.edit", Label: "toggle"})
	case f.kind == areaField:
		out = append(out, wm.Hint{Action: "config.app.edit"})
	case typable(f):
		out = append(out, wm.Hint{Action: "config.app.left", Pair: "config.app.right", Label: "change"}, wm.Hint{Action: "config.app.edit"})
	default:
		out = append(out, wm.Hint{Action: "config.app.left", Pair: "config.app.right", Label: "change"})
	}
	if f.optional && f.value != "" {
		out = append(out, wm.Hint{Action: "config.app.unset"})
	}
	return out
}
