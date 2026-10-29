package cards

import (
	"math"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Rows a multi line field shows, its editor scrolling past them
const areaRows = 8

// Columns between two pills of a choice
const chipGap = 1

// Longest a slider track grows
const trackMax = 40

// Share of the way from the surface to the ghost color an unfocused well takes
const wellTint = 0.6

// Meaning of a hit that lands off a choice, the well, the slider track or the switch
const (
	hitBox   = -1
	hitTrack = -2
	hitFlip  = -3
)

// Clickable stretch of a rendered control, in the rows and columns of its value
type hit struct {
	row, x0, x1 int
	choice      int
}

// Widths the number wells of one sheet share so their boxes, placeholders, range ends and tracks line up
type sliderCols struct {
	box, wide, lo, hi int
}

// How a control paints, lit while focused and showing the cursor while typing
type look struct {
	focused bool
	typing  bool
}

// Background of a well, lit while focused, faint otherwise and bare on a theme that cannot blend
func (lk look) well(t theme.Theme) lipgloss.Style {
	switch {
	case lk.focused:
		return lipgloss.NewStyle().Background(t.Ghost)
	case t.Plain:
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Background(theme.Mix(t.Surface, t.Ghost, wellTint))
}

// Text an empty control shows, the placeholder or a dash where no well marks the control
func (lk look) empty(placeholder string, t theme.Theme) string {
	if placeholder == "" && !lk.focused && t.Plain {
		return "-"
	}
	return placeholder
}

// Section title in capitals behind the accent notch with a rule running to width
func heading(title string, w int, t theme.Theme) string {
	notch := lipgloss.NewStyle().Foreground(t.Accent).Render(t.Glyphs.Notch)
	label := lipgloss.NewStyle().Foreground(t.TextDim).Bold(true).Render(strings.ToUpper(title))
	rest := w - ansi.StringWidth(label) - 3
	return notch + " " + label + " " + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, max(rest, 0)))
}

// Number well widths over every number field, shown or hidden, so the columns hold still as fields come and go
func sliderColumns(fields []*field) sliderCols {
	c := sliderCols{box: 4}
	for _, f := range fields {
		if f.kind != numberField {
			continue
		}
		c.box = max(c.box, f.boxWidth())
		if !f.bounded() {
			c.wide = max(c.wide, c.box, ansi.StringWidth(f.placeholder))
			continue
		}
		c.lo = max(c.lo, ansi.StringWidth(f.mark(f.lo)))
		c.hi = max(c.hi, ansi.StringWidth(f.mark(f.hi)))
	}
	return c
}

// Rows of the value at width with its clicks in the look given, and the cursor cell while typing
func (f *field) render(w int, lk look, cols sliderCols, t theme.Theme) ([]string, []hit, int, int) {
	switch f.kind {
	case choiceField:
		if f.toggle() {
			row, hits := f.switchView(lk, t)
			return []string{row}, hits, -1, 0
		}
		rows, hits := chips(f.choices, f.value, lk.focused, w, t)
		return rows, hits, -1, 0
	case numberField:
		row, hits, x := f.slider(w, lk, cols, t)
		return []string{row}, hits, x, 0
	case areaField:
		return f.areaView(w, lk, t)
	}
	row, x := f.box(w, lk, t)
	return []string{row}, []hit{{row: 0, x0: 0, x1: w, choice: hitBox}}, x, 0
}

// Choices as pills in one row or aligned columns, the picked one lit, with the hit of each
func chips(choices []string, picked string, focused bool, w int, t theme.Theme) ([]string, []hit) {
	n := len(choices)
	if n == 0 {
		return []string{""}, nil
	}
	cell, total := 0, -chipGap
	for _, c := range choices {
		cw := ansi.StringWidth(c) + 2
		cell = max(cell, cw)
		total += cw + chipGap
	}
	cols := n
	if total > w {
		cols = max((w+chipGap)/(cell+chipGap), 1)
	}
	faint := look{}.well(t)
	var rows []string
	var hits []hit
	for start := 0; start < n; start += cols {
		var sb strings.Builder
		x := 0
		for i := start; i < min(start+cols, n); i++ {
			label := " " + choices[i] + " "
			if cols < n {
				label = render.Fit(label, cell, t)
			}
			lw := ansi.StringWidth(label)
			st := faint.Foreground(t.TextDim)
			switch {
			case choices[i] == picked && focused:
				st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
			case choices[i] == picked:
				st = lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true)
			case focused:
				st = faint.Foreground(t.Text)
			}
			if x > 0 {
				sb.WriteString(strings.Repeat(" ", chipGap))
				x += chipGap
			}
			sb.WriteString(st.Render(label))
			hits = append(hits, hit{row: len(rows), x0: x, x1: x + lw, choice: i})
			x += lw
		}
		rows = append(rows, sb.String())
	}
	return rows, hits
}

// Switch with its side named, lit in ice while on and dim while off, clickable along its length
func (f *field) switchView(lk look, t theme.Theme) (string, []hit) {
	g := t.Glyphs
	var row string
	if f.on() {
		track := lipgloss.NewStyle().Foreground(t.Ice).Bold(lk.focused).Render(g.SwitchOn)
		row = track + " " + lipgloss.NewStyle().Foreground(t.Text).Bold(lk.focused).Render("on")
	} else {
		fg := t.Steel
		if lk.focused {
			fg = t.SteelBright
		}
		track := lipgloss.NewStyle().Foreground(fg).Bold(lk.focused).Render(g.SwitchOff)
		row = track + " " + lipgloss.NewStyle().Foreground(t.TextDim).Render("off")
	}
	return row, []hit{{row: 0, x0: 0, x1: ansi.StringWidth(row), choice: hitFlip}}
}

// Text as an input well at width, masked when secret and scrolled to the cursor, with the cursor column
func (f *field) box(w int, lk look, t theme.Theme) (string, int) {
	base := lk.well(t)
	room := max(w-2, 1)
	runes := []rune(strings.Map(oneLine, f.value))
	if f.kind == secretField {
		runes = []rune(strings.Repeat(t.Glyphs.Mask, len(runes)))
	}
	if len(runes) == 0 {
		x := -1
		if lk.typing {
			x = 1
		}
		hint := render.Fit(lk.empty(f.placeholder, t), room, t)
		return base.Render(" ") + base.Foreground(t.TextDim).Render(hint) + base.Render(" "), x
	}
	start := 0
	if f.cursor > room-1 {
		start = f.cursor - room + 1
	}
	shown := string(runes[start:min(len(runes), start+room)])
	x := -1
	if lk.typing {
		x = 1 + f.cursor - start
	}
	return base.Render(" ") + base.Foreground(t.Text).Render(render.Fit(shown, room, t)) + base.Render(" "), x
}

// Line break shown as a space so a value holding several lines fits its one row
func oneLine(r rune) rune {
	if r == '\n' || r == '\r' {
		return ' '
	}
	return r
}

// Number well with its hits and cursor column, range ends and a track following when the number has a ceiling
func (f *field) slider(w int, lk look, cols sliderCols, t theme.Theme) (string, []hit, int) {
	inner := max(cols.box, f.boxWidth())
	if !f.bounded() {
		inner = max(inner, cols.wide, ansi.StringWidth(f.placeholder))
	}
	bw := inner + 2
	base := lk.well(t)
	text := base.Foreground(t.Text).Render(render.Fit(f.value, inner, t))
	if f.value == "" {
		text = base.Foreground(t.TextDim).Render(render.Fit(or(f.placeholder, "-"), inner, t))
	}
	box := base.Render(" ") + text + base.Render(" ")
	x := -1
	if lk.typing {
		x = 1 + min(f.cursor, inner)
	}
	hits := []hit{{row: 0, x0: 0, x1: bw, choice: hitBox}}
	if !f.bounded() {
		return box, hits, x
	}
	lo, hi := f.mark(f.lo), f.mark(f.hi)
	loW, hiW := max(cols.lo, ansi.StringWidth(lo)), max(cols.hi, ansi.StringWidth(hi))
	tw := min(w-bw-2-loW-hiW-2, trackMax)
	if tw < 6 {
		return box, hits, x
	}
	fill, thumb, rest := t.Glyphs.Fill, t.Glyphs.Thumb, t.Glyphs.Slide
	lit := t.SteelBright
	if lk.focused {
		lit = t.Ice
	}
	at := -1
	if v, ok := f.number(); ok {
		at = int(math.Round((min(max(v, f.lo), f.hi) - f.lo) / (f.hi - f.lo) * float64(tw-1)))
	}
	var sb strings.Builder
	for i := range tw {
		switch {
		case i < at:
			sb.WriteString(lipgloss.NewStyle().Foreground(lit).Render(fill))
		case i == at:
			sb.WriteString(lipgloss.NewStyle().Foreground(lit).Bold(true).Render(thumb))
		default:
			sb.WriteString(lipgloss.NewStyle().Foreground(t.Steel).Render(rest))
		}
	}
	tx := bw + 2 + loW + 1
	hits = append(hits, hit{row: 0, x0: tx, x1: tx + tw, choice: hitTrack})
	dim := lipgloss.NewStyle().Foreground(t.TextDim)
	return box + "  " + dim.Render(strings.Repeat(" ", loW-ansi.StringWidth(lo))+lo) + " " + sb.String() + " " + dim.Render(render.Fit(hi, hiW, t)), hits, x
}

// Editor of a multi line field on the composer keys, loaded with the value
func newArea(env *Env, value string) *textarea.Model {
	ta := textarea.New()
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.SetVirtualCursor(false)
	ta.KeyMap = editingKeys(env.Keys)
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("enter", "ctrl+j"))
	ta.SetStyles(editorStyles(env.Theme))
	ta.SetHeight(areaRows)
	ta.SetValue(value)
	return &ta
}

// Rows of the editor in a well at width, each clickable, with the cursor cell while typing
func (f *field) areaView(w int, lk look, t theme.Theme) ([]string, []hit, int, int) {
	a := f.area
	if a.Value() != f.value {
		a.SetValue(f.value)
		f.value = a.Value()
	}
	room := max(w-2, 1)
	a.SetWidth(room)
	a.SetHeight(areaRows)
	a.Placeholder = lk.empty(f.placeholder, t)
	base := lk.well(t)
	st := editorStyles(t)
	for _, s := range []*textarea.StyleState{&st.Focused, &st.Blurred} {
		s.Base = s.Base.Inherit(base)
		s.Text = s.Text.Inherit(base)
		s.CursorLine = s.CursorLine.Inherit(base)
		s.EndOfBuffer = s.EndOfBuffer.Inherit(base)
		s.Placeholder = lipgloss.NewStyle().Foreground(t.TextDim).Inherit(base)
	}
	a.SetStyles(st)
	lines := strings.Split(a.View(), "\n")
	rows := make([]string, 0, areaRows)
	hits := make([]hit, 0, areaRows)
	for i := range areaRows {
		text := ""
		if i < len(lines) {
			text = lines[i]
		}
		rows = append(rows, base.Render(" ")+render.Fit(text, room, t)+base.Render(" "))
		hits = append(hits, hit{row: i, x0: 0, x1: w, choice: hitBox})
	}
	x, y := -1, 0
	if lk.typing {
		if cur := a.Cursor(); cur != nil {
			x, y = cur.X+1, cur.Y
		}
	}
	return rows, hits, x, y
}

// Puts the editor cursor on the row and column clicked
func (f *field) placeCursor(row, col int) {
	a := f.area
	skip := a.ScrollYOffset() + row
	a.MoveToBegin()
	for range skip {
		a.CursorDown()
	}
	li := a.LineInfo()
	a.SetCursorColumn(li.StartColumn + min(max(col, 0), li.Width))
}
