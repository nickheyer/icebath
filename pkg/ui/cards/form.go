package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Width a dialog asks for, the overlay narrowing it on a small screen
const formWidth = 88

// Composer action whose keys the editor of a multi line field answers for a form editing action
var areaEdits = map[string]string{
	"form.left":        "composer.left",
	"form.right":       "composer.right",
	"form.delete.back": "composer.delete.back",
	"form.delete.word": "composer.delete.word.back",
	"form.home":        "composer.line.start",
	"form.end":         "composer.line.end",
}

// Buttons along the bottom of a dialog
const (
	noButton = iota
	saveButton
	cancelButton
)

// Dialog of fields over the desk that saves through submit and stays open showing why when the save fails
type Form struct {
	env     *Env
	title   string
	glyph   string
	sheet   *sheet
	err     string
	submit  func() (tea.Cmd, error)
	buttons []formHit
}

// Builds a dialog over the fields, focused on the first that shows
func newForm(env *Env, title, glyph string, fields []*field, submit func() (tea.Cmd, error)) *Form {
	f := &Form{env: env, title: title, glyph: glyph, sheet: newSheet(env, fields), submit: submit}
	f.sheet.typing = true
	return f
}

func (f *Form) Title() string       { return f.title }
func (f *Form) Glyph() string       { return f.glyph }
func (f *Form) Key() string         { return "form:" + f.title }
func (f *Form) Init() tea.Cmd       { return nil }
func (f *Form) Cursor() *tea.Cursor { return f.sheet.cur }

// Dialog keys, under the editor keys while a multi line field has the focus
func (f *Form) Contexts() []string {
	if fd := f.sheet.focused(); fd != nil && fd.kind == areaField {
		return []string{keys.Area, keys.Form}
	}
	return []string{keys.Form}
}

// Hands a key or paste to the editor of a multi line field and takes its text back as the value
func (f *Form) typeArea(fd *field, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	*fd.area, cmd = fd.area.Update(msg)
	fd.value = fd.area.Value()
	fd.cursor = len([]rune(fd.value))
	delete(f.sheet.bad, fd)
	return cmd
}

func (f *Form) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	f.sheet.prepare()
	fd := f.sheet.focused()
	f.sheet.aim()
	switch msg := msg.(type) {
	case wm.ActionMsg:
		return f, f.act(msg)
	case tea.KeyPressMsg:
		if fd != nil && fd.kind == areaField {
			return f, f.typeArea(fd, msg)
		}
		if fd != nil && msg.Text != "" {
			fd.typeText(msg.Text)
			delete(f.sheet.bad, fd)
		}
	case tea.PasteMsg:
		if fd != nil && fd.kind == areaField {
			return f, f.typeArea(fd, msg)
		}
		if fd != nil {
			fd.typeText(msg.Content)
			delete(f.sheet.bad, fd)
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return f, nil
		}
		return f, f.click(msg.X, msg.Y)
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			f.sheet.wheel(-1)
		} else {
			f.sheet.wheel(1)
		}
	}
	return f, nil
}

// Presses a button, pages the sheet, or focuses the field under the click and applies the hit on its control
func (f *Form) click(x, y int) tea.Cmd {
	for _, b := range f.buttons {
		if y != b.y || x < b.x0 || x >= b.x1 {
			continue
		}
		if b.button == saveButton {
			return f.save()
		}
		return wm.RequestDismissOf(f)
	}
	if f.sheet.page(x, y) {
		return nil
	}
	i, h := f.sheet.hitAt(x, y)
	if i < 0 {
		return nil
	}
	f.sheet.at = i
	if h != nil {
		h.field.click(h.hit, x-h.x0+h.hit.x0)
		delete(f.sheet.bad, h.field)
	}
	return nil
}

// Carries out a dialog action on the focus, the focused field, or the dialog as a whole
func (f *Form) act(msg wm.ActionMsg) tea.Cmd {
	s := f.sheet
	fd := s.focused()
	id := msg.ID
	if fd != nil && fd.kind == areaField {
		if cmd, taken := f.actArea(fd, id); taken {
			return cmd
		}
	}
	switch id {
	case "form.next":
		s.at = s.step(s.at, 1)
	case "form.prev":
		s.at = s.step(s.at, -1)
	case "form.submit":
		return f.save()
	case "form.cancel":
		return wm.RequestDismissOf(f)
	case "form.left", "form.right":
		if fd != nil {
			dir := 1
			if id == "form.left" {
				dir = -1
			}
			fd.nudge(dir)
			delete(s.bad, fd)
		}
	case "form.unset":
		if fd != nil && fd.unset() {
			delete(s.bad, fd)
		}
	default:
		if fd != nil && strings.HasPrefix(id, "form.") && fd.edit(strings.TrimPrefix(id, "form.")) {
			delete(s.bad, fd)
		}
	}
	return nil
}

// Carries out an action on the editor of the focused multi line field, reporting whether it was one for it
func (f *Form) actArea(fd *field, id string) (tea.Cmd, bool) {
	a := fd.area
	s := f.sheet
	if alias, ok := areaEdits[id]; ok {
		if press := keys.Press(f.env.Keys.First(alias)); press.Code != 0 {
			return f.typeArea(fd, press), true
		}
		return nil, true
	}
	switch id {
	case "area.newline":
		a.InsertString("\n")
	case "area.save":
		return f.save(), true
	case "area.up":
		if a.Line() == 0 && a.LineInfo().RowOffset == 0 {
			s.at = s.step(s.at, -1)
			return nil, true
		}
		a.CursorUp()
	case "area.down":
		if li := a.LineInfo(); a.Line() == a.LineCount()-1 && li.RowOffset >= li.Height-1 {
			s.at = s.step(s.at, 1)
			return nil, true
		}
		a.CursorDown()
	case "form.clear":
		a.SetValue("")
	default:
		return nil, false
	}
	fd.value = a.Value()
	fd.cursor = len([]rune(fd.value))
	delete(s.bad, fd)
	return nil, true
}

// Checks every shown field and saves, closing the dialog unless something refused
func (f *Form) save() tea.Cmd {
	s := f.sheet
	s.bad = map[*field]string{}
	f.err = ""
	first := -1
	for i, fd := range s.fields {
		if !fd.visible() {
			continue
		}
		if p := fd.problem(); p != "" {
			s.bad[fd] = p
			if first < 0 {
				first = i
			}
		}
	}
	if first >= 0 {
		s.at = first
		return nil
	}
	cmd, err := f.submit()
	if err != nil {
		f.err = err.Error()
		return nil
	}
	return tea.Batch(wm.RequestDismissOf(f), cmd)
}

// Rule under the fields, any save error, and the buttons right aligned with their clicks
func (f *Form) footer(w int) ([]string, []formHit) {
	t := f.env.Theme
	indent := strings.Repeat(" ", sheetGutter)
	foot := []string{"", lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, w))}
	if f.err != "" {
		for _, line := range strings.Split(render.Wrap(t.Glyphs.Fail+" "+f.err, max(w-sheetGutter, 1)), "\n") {
			foot = append(foot, indent+render.Bad(line, t))
		}
	}
	save := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true).Render(" " + t.Glyphs.Check + " save ")
	cancel := lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Render(" " + t.Glyphs.Close + " cancel ")
	sw, cw := ansi.StringWidth(save), ansi.StringWidth(cancel)
	pad := max(w-sw-cw-2-sheetGutter, 0)
	hits := []formHit{{y: -1, x0: pad, x1: pad + sw, button: saveButton}, {y: -1, x0: pad + sw + 2, x1: pad + sw + 2 + cw, button: cancelButton}}
	return append(foot, strings.Repeat(" ", pad)+save+"  "+cancel), hits
}

// Fields over the footer, the buttons placed on the last row
func (f *Form) View(w, h int) string {
	foot, buttons := f.footer(w)
	room := max(h-len(foot), 1)
	body := f.sheet.view(w, room, f.env.Theme)
	f.buttons = f.buttons[:0]
	for _, b := range buttons {
		b.y = room + len(foot) - 1
		f.buttons = append(f.buttons, b)
	}
	return body + "\n" + strings.Join(foot, "\n")
}

// Size the dialog asks for, every field at the dialog width over the help and the footer
func (f *Form) MinSize() (int, int) {
	foot, _ := f.footer(formWidth)
	return formWidth, f.sheet.height(formWidth, f.env.Theme) + len(foot)
}

// Actions the dialog answers, choosing keys on a choice or number, editor keys on a multi line field
func (f *Form) Hints() []wm.Hint {
	out := []wm.Hint{{Action: "form.submit"}, {Action: "form.cancel"}, {Action: "form.next", Pair: "form.prev", Label: "field"}}
	if fd := f.sheet.focused(); fd != nil {
		if fd.kind == areaField {
			out = []wm.Hint{{Action: "area.save"}, {Action: "form.cancel"}, {Action: "area.newline"}, {Action: "form.next", Pair: "form.prev", Label: "field"}}
		}
		switch {
		case fd.toggle():
			out = append(out, wm.Hint{Action: "form.left", Pair: "form.right", Label: "toggle"})
		case fd.kind == choiceField || fd.kind == numberField:
			out = append(out, wm.Hint{Action: "form.left", Pair: "form.right", Label: "change"})
		}
		if fd.optional {
			out = append(out, wm.Hint{Action: "form.unset"})
		}
	}
	return out
}
