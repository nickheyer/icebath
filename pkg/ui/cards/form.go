package cards

import (
	"math"
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

// Columns left of each label holding the bar that marks the focused field
const formGutter = 2

// Buttons along the bottom of a dialog
const (
	noButton = iota
	saveButton
	cancelButton
)

// Clickable stretch of a dialog row, a field value or a button
type formHit struct {
	y, x0, x1 int
	field     *field
	hit       hit
	button    int
}

// Dialog laid out at one width and height, its field rows over the foot with the clicks and cursor they place
type formLayout struct {
	body  []string
	foot  []string
	spans [][2]int
	hits  []formHit
	cur   *tea.Cursor
}

// Dialog of fields over the desk that saves through submit and stays open showing why when the save fails
type Form struct {
	env    *Env
	title  string
	glyph  string
	fields []*field
	at     int
	err    string
	bad    map[*field]string
	submit func() (tea.Cmd, error)
	hits   []formHit
	scroll int
	cur    *tea.Cursor
}

// Builds a dialog over the fields, focused on the first that shows
func newForm(env *Env, title, glyph string, fields []*field, submit func() (tea.Cmd, error)) *Form {
	f := &Form{env: env, title: title, glyph: glyph, fields: fields, submit: submit, bad: map[*field]string{}}
	f.at = f.step(-1, 1)
	return f
}

func (f *Form) Title() string           { return f.title }
func (f *Form) Glyph() string           { return f.glyph }
func (f *Form) Key() string             { return "form:" + f.title }
func (f *Form) Init() tea.Cmd           { return nil }
func (f *Form) Contexts() []string      { return []string{keys.Form} }
func (f *Form) Cursor() *tea.Cursor     { return f.cur }
func (f *Form) field(key string) *field { return fieldOf(f.fields, key) }

// Index of the next shown field from i in the direction, i itself when none other shows
func (f *Form) step(i, dir int) int {
	n := len(f.fields)
	for k := 1; k <= n; k++ {
		j := ((i+dir*k)%n + n) % n
		if f.fields[j].visible() {
			return j
		}
	}
	return max(i, 0)
}

// Field with the focus, nil when the dialog has none
func (f *Form) focused() *field {
	if f.at < 0 || f.at >= len(f.fields) || !f.fields[f.at].visible() {
		f.at = f.step(f.at, 1)
	}
	if f.at >= len(f.fields) {
		return nil
	}
	return f.fields[f.at]
}

func (f *Form) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	fd := f.focused()
	switch msg := msg.(type) {
	case wm.ActionMsg:
		return f, f.act(msg.ID)
	case tea.KeyPressMsg:
		if fd != nil && msg.Text != "" {
			fd.typeText(msg.Text)
			delete(f.bad, fd)
		}
	case tea.PasteMsg:
		if fd != nil {
			fd.typeText(msg.Content)
			delete(f.bad, fd)
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return f, nil
		}
		for _, h := range f.hits {
			if msg.Y != h.y || msg.X < h.x0 || msg.X >= h.x1 {
				continue
			}
			switch h.button {
			case saveButton:
				return f, f.save()
			case cancelButton:
				return f, wm.RequestDismissOf(f)
			}
			for i, other := range f.fields {
				if other == h.field {
					f.at = i
				}
			}
			h.field.click(h.hit, msg.X-h.x0+h.hit.x0)
			delete(f.bad, h.field)
			return f, nil
		}
	case tea.MouseWheelMsg:
		if msg.Button == tea.MouseWheelUp {
			f.at = f.step(f.at, -1)
		} else {
			f.at = f.step(f.at, 1)
		}
	}
	return f, nil
}

// Carries out a dialog action on the focus, the focused field, or the dialog as a whole
func (f *Form) act(id string) tea.Cmd {
	fd := f.focused()
	switch id {
	case "form.next":
		f.at = f.step(f.at, 1)
	case "form.prev":
		f.at = f.step(f.at, -1)
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
			delete(f.bad, fd)
		}
	case "form.unset":
		if fd != nil && fd.unset() {
			delete(f.bad, fd)
		}
	default:
		if fd != nil && strings.HasPrefix(id, "form.") && fd.edit(strings.TrimPrefix(id, "form.")) {
			delete(f.bad, fd)
		}
	}
	return nil
}

// Checks every shown field and saves, closing the dialog unless something refused
func (f *Form) save() tea.Cmd {
	f.bad = map[*field]string{}
	f.err = ""
	first := -1
	for i, fd := range f.fields {
		if !fd.visible() {
			continue
		}
		if p := fd.problem(); p != "" {
			f.bad[fd] = p
			if first < 0 {
				first = i
			}
		}
	}
	if first >= 0 {
		f.at = first
		return nil
	}
	cmd, err := f.submit()
	if err != nil {
		f.err = err.Error()
		return nil
	}
	return tea.Batch(wm.RequestDismissOf(f), cmd)
}

// Dialog at width within height h, fields beside labels with the focus barred, leaving the dialog untouched
func (f *Form) layout(w, h int) formLayout {
	t := f.env.Theme
	lw := 0
	for _, fd := range f.fields {
		lw = max(lw, ansi.StringWidth(fd.label))
	}
	lw = min(lw+3, max(w/3, 8))
	left := formGutter + lw
	vw := max(w-left, 8)
	cols := sliderColumns(f.fields)
	type part struct {
		i     int
		fd    *field
		lines []string
		hits  []hit
		cx    int
	}
	// Rows of one field in the look given, its problem wrapped under the value
	paint := func(i int, fd *field, framed bool) part {
		focus := i == f.at
		rows, hits, cx := fd.render(vw, look{focused: focus, typing: focus, framed: framed}, cols, t)
		st := lipgloss.NewStyle().Foreground(t.TextDim)
		switch {
		case f.bad[fd] != "":
			st = lipgloss.NewStyle().Foreground(t.Alarm).Bold(focus)
		case focus:
			st = lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
		}
		lines := make([]string, 0, len(rows)+1)
		for r, row := range rows {
			label := ""
			if r == 0 {
				label = st.Render(render.Truncate(fd.label, lw-2))
			}
			lines = append(lines, render.Fit(label, lw)+row)
		}
		if p := f.bad[fd]; p != "" {
			for _, line := range strings.Split(render.Wrap(p, vw), "\n") {
				lines = append(lines, render.Fit("", lw)+render.Bad(line, t))
			}
		}
		return part{i: i, fd: fd, lines: lines, hits: hits, cx: cx}
	}
	var parts []part
	var blocks []stack
	used := 0
	for i, fd := range f.fields {
		if fd.visible() {
			p := paint(i, fd, true)
			parts = append(parts, p)
			blocks = append(blocks, stack{rows: len(p.lines), well: fd.welled()})
			used += len(p.lines)
		}
	}
	foot, buttons := f.footer(w)
	spare := h - used - len(foot)
	gaps := spaceOut(blocks, spare)
	for _, g := range gaps {
		if g {
			spare--
		}
	}
	if spare > 0 && len(parts) > 0 {
		foot = append([]string{""}, foot...)
	}
	if touching(blocks, gaps) {
		for k, p := range parts {
			if p.fd.welled() && p.i != f.at {
				parts[k] = paint(p.i, p.fd, false)
			}
		}
	}
	bar := "┃"
	if t.Plain {
		bar = "|"
	}
	bar = lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(bar) + strings.Repeat(" ", formGutter-1)
	out := formLayout{spans: make([][2]int, len(f.fields)), foot: foot}
	for i := range out.spans {
		out.spans[i] = [2]int{-1, -1}
	}
	for k, p := range parts {
		if gaps[k] {
			out.body = append(out.body, "")
		}
		gutter := strings.Repeat(" ", formGutter)
		if p.i == f.at {
			gutter = bar
		}
		top := len(out.body)
		for _, line := range p.lines {
			out.body = append(out.body, gutter+line)
		}
		for _, h := range p.hits {
			out.hits = append(out.hits, formHit{y: top + h.row, x0: left + h.x0, x1: left + h.x1, field: p.fd, hit: h})
		}
		if p.i == f.at && p.cx >= 0 {
			out.cur = tea.NewCursor(left+p.cx, top)
		}
		out.spans[p.i] = [2]int{top, len(out.body)}
	}
	out.hits = append(out.hits, buttons...)
	return out
}

// Rule, help of the focus in room for the longest help, any save error, and buttons with their clicks
func (f *Form) footer(w int) ([]string, []formHit) {
	t := f.env.Theme
	foot := []string{lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat("─", w))}
	hw := max(w-formGutter, 1)
	indent := strings.Repeat(" ", formGutter)
	room := 0
	for _, fd := range f.fields {
		if fd.visible() && fd.help != "" {
			room = max(room, strings.Count(render.Wrap(fd.help, hw), "\n")+1)
		}
	}
	var help []string
	if fd := f.focused(); fd != nil && fd.help != "" {
		help = strings.Split(render.Wrap(fd.help, hw), "\n")
	}
	for i := range room {
		line := ""
		if i < len(help) {
			line = indent + render.Dim(help[i], t)
		}
		foot = append(foot, line)
	}
	if f.err != "" {
		for _, line := range strings.Split(render.Wrap(f.err, hw), "\n") {
			foot = append(foot, indent+render.Bad(line, t))
		}
	}
	save := lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true).Render("  save  ")
	cancel := lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Render("  cancel  ")
	sw, cw := ansi.StringWidth(save), ansi.StringWidth(cancel)
	pad := max(w-sw-cw-2, 0)
	hits := []formHit{{y: -1, x0: pad, x1: pad + sw, button: saveButton}, {y: -1, x0: pad + sw + 2, x1: pad + sw + 2 + cw, button: cancelButton}}
	return append(foot, "", strings.Repeat(" ", pad)+save+"  "+cancel), hits
}

// Dialog spaced out to fill the height, scrolled to keep the focus in view when even unspaced it runs taller
func (f *Form) View(w, h int) string {
	lay := f.layout(w, h)
	room := max(h-len(lay.foot), 1)
	if fd := f.focused(); fd != nil {
		s := lay.spans[f.at]
		if s[0] < f.scroll {
			f.scroll = s[0]
		}
		if s[1] > f.scroll+room {
			f.scroll = s[1] - room
		}
	}
	f.scroll = min(max(f.scroll, 0), max(len(lay.body)-room, 0))
	shown := lay.body[min(f.scroll, len(lay.body)):]
	shown = shown[:min(len(shown), room)]
	f.hits = f.hits[:0]
	for _, hh := range lay.hits {
		switch {
		case hh.button != noButton:
			hh.y = room + len(lay.foot) - 1
		case hh.y >= f.scroll && hh.y < f.scroll+room:
			hh.y -= f.scroll
		default:
			continue
		}
		f.hits = append(f.hits, hh)
	}
	f.cur = lay.cur
	if f.cur != nil {
		f.cur.Y -= f.scroll
		if f.cur.Y < 0 || f.cur.Y >= room {
			f.cur = nil
		}
	}
	for len(shown) < room {
		shown = append(shown, "")
	}
	return strings.Join(append(shown, lay.foot...), "\n")
}

// Size the dialog asks for, every field spaced apart at the dialog width over the help, errors and buttons
func (f *Form) MinSize() (int, int) {
	lay := f.layout(formWidth, math.MaxInt32)
	return formWidth, len(lay.body) + len(lay.foot)
}

// Actions the dialog answers, the choosing keys while a choice or number has the focus
func (f *Form) Hints() []wm.Hint {
	out := []wm.Hint{{Action: "form.submit"}, {Action: "form.cancel"}, {Action: "form.next", Pair: "form.prev", Label: "field"}}
	if fd := f.focused(); fd != nil {
		if fd.kind == choiceField || fd.kind == numberField {
			out = append(out, wm.Hint{Action: "form.left", Pair: "form.right", Label: "change"})
		}
		if fd.optional {
			out = append(out, wm.Hint{Action: "form.unset"})
		}
	}
	return out
}
