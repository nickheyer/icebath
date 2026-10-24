package cards

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Kind of value a field takes
type fieldKind int

const (
	textField fieldKind = iota
	secretField
	choiceField
	numberField
)

// Choice index a hit stands for when it lands on the number box or the slider track
const (
	hitBox   = -1
	hitTrack = -2
)

// Share of the way from the surface to the ghost color an unfocused input well takes
const wellTint = 0.45

// Clickable stretch of a rendered field, in the rows and columns of its value
type hit struct {
	row, x0, x1 int
	choice      int
}

// Widths the sliders of one list share so their boxes, range ends and tracks line up in columns
type sliderCols struct {
	box, lo, hi int
}

// How a field paints, lit while focused, showing the cursor while typing, and in a faint well while framed
type look struct {
	focused bool
	typing  bool
	framed  bool
}

// Height of one laid out field and whether it paints in a well
type stack struct {
	rows int
	well bool
}

// Blank row before each block within spare rows, around wrapped blocks first, then every pair once all fit
func spaceOut(blocks []stack, spare int) []bool {
	gaps := make([]bool, len(blocks))
	var wraps, rest []int
	for i := 1; i < len(blocks); i++ {
		if blocks[i-1].rows > 1 || blocks[i].rows > 1 {
			wraps = append(wraps, i)
		} else {
			rest = append(rest, i)
		}
	}
	k := min(max(spare, 0), len(wraps))
	for j := range k {
		gaps[wraps[(2*j+1)*len(wraps)/(2*k)]] = true
	}
	if spare-k >= len(rest) {
		for _, i := range rest {
			gaps[i] = true
		}
	}
	return gaps
}

// Reports whether two wells touch with no blank row between them, which would merge them into one slab
func touching(blocks []stack, gaps []bool) bool {
	for i := 1; i < len(blocks); i++ {
		if blocks[i-1].well && blocks[i].well && !gaps[i] {
			return true
		}
	}
	return false
}

// Reports whether the field paints its value in a well
func (f *field) welled() bool { return f.kind != choiceField }

// One input of a dialog or a settings row, its value kept as text and empty while unset
type field struct {
	key         string
	label       string
	help        string
	placeholder string
	kind        fieldKind
	choices     []string
	value       string
	cursor      int
	lo, hi      float64
	step        float64
	start       float64
	integer     bool
	optional    bool
	unit        func(float64) string
	show        func() bool
	check       func(string) error
	onChange    func()
}

// Reports whether the field shows right now
func (f *field) visible() bool { return f.show == nil || f.show() }

// Number the text holds, false while unset or unparsable
func (f *field) number() (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(f.value), 64)
	return v, err == nil
}

// Text of a number at the precision of the step
func (f *field) format(v float64) string {
	if f.integer {
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	return strconv.FormatFloat(v, 'f', decimals(f.step), 64)
}

// Places after the point a step needs, at most six
func decimals(step float64) int {
	for d := range 6 {
		scaled := step * math.Pow(10, float64(d))
		if math.Abs(scaled-math.Round(scaled)) < 1e-9 {
			return d
		}
	}
	return 6
}

// Range end as the slider labels it
func (f *field) mark(v float64) string {
	if f.unit != nil {
		return f.unit(v)
	}
	return f.format(v)
}

// Holds the multiple of the step nearest v, kept inside the range
func (f *field) setNumber(v float64) {
	if f.step > 0 {
		v = math.Round(v/f.step) * f.step
	}
	f.set(f.format(min(max(v, f.lo), f.hi)))
}

// Replaces the value with the cursor at its end and tells the listener
func (f *field) set(v string) {
	f.value = v
	f.cursor = len([]rune(v))
	if f.onChange != nil {
		f.onChange()
	}
}

// Picks the neighbouring choice, steps the number from its start when unset, or moves the cursor
func (f *field) nudge(dir int) {
	switch f.kind {
	case choiceField:
		if len(f.choices) == 0 {
			return
		}
		i := slices.Index(f.choices, f.value)
		j := 0
		if i >= 0 {
			j = min(max(i+dir, 0), len(f.choices)-1)
		}
		if f.choices[j] != f.value {
			f.set(f.choices[j])
		}
	case numberField:
		if v, ok := f.number(); ok {
			f.setNumber(v + float64(dir)*f.step)
			return
		}
		f.setNumber(f.start)
	default:
		f.cursor = min(max(f.cursor+dir, 0), len([]rune(f.value)))
	}
}

// Inserts typed text at the cursor, a choice field jumping to the next choice the text starts
func (f *field) typeText(text string) {
	text = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
	if f.kind == choiceField {
		n := len(f.choices)
		from := slices.Index(f.choices, f.value)
		for k := 1; k <= n; k++ {
			c := f.choices[((from+k)%n+n)%n]
			if strings.HasPrefix(strings.ToLower(c), strings.ToLower(text)) {
				f.set(c)
				return
			}
		}
		return
	}
	if f.kind == numberField {
		text = strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' || r == '.' || r == '-' {
				return r
			}
			return -1
		}, text)
	}
	if text == "" {
		return
	}
	r := []rune(f.value)
	at := min(f.cursor, len(r))
	f.value = string(r[:at]) + text + string(r[at:])
	f.cursor = at + len([]rune(text))
	if f.onChange != nil {
		f.onChange()
	}
}

// Edits the text around the cursor by a form editing action, reporting whether it was one
func (f *field) edit(id string) bool {
	if f.kind == choiceField {
		return false
	}
	r := []rune(f.value)
	at := min(f.cursor, len(r))
	switch id {
	case "delete.back":
		if at == 0 {
			return true
		}
		f.value, f.cursor = string(r[:at-1])+string(r[at:]), at-1
	case "delete.word":
		i := at
		for i > 0 && r[i-1] == ' ' {
			i--
		}
		for i > 0 && r[i-1] != ' ' {
			i--
		}
		f.value, f.cursor = string(r[:i])+string(r[at:]), i
	case "clear":
		f.value, f.cursor = "", 0
	case "home":
		f.cursor = 0
		return true
	case "end":
		f.cursor = len(r)
		return true
	default:
		return false
	}
	if f.onChange != nil {
		f.onChange()
	}
	return true
}

// Leaves an optional field empty, reporting whether it is one
func (f *field) unset() bool {
	if !f.optional {
		return false
	}
	f.set("")
	return true
}

// Problem with the value, a required field left empty or what its check refuses
func (f *field) problem() string {
	v := strings.TrimSpace(f.value)
	if v == "" {
		if f.optional || f.kind == choiceField {
			return ""
		}
		return f.label + " is required"
	}
	if f.kind == numberField {
		n, ok := f.number()
		if !ok {
			return f.label + " must be a number"
		}
		if n < f.lo || n > f.hi {
			return f.label + " must lie between " + f.format(f.lo) + " and " + f.format(f.hi)
		}
		if f.integer && n != math.Trunc(n) {
			return f.label + " must be a whole number"
		}
	}
	if f.check != nil {
		if err := f.check(v); err != nil {
			return err.Error()
		}
	}
	return ""
}

// Slider widths over every number field, shown or hidden, so the columns hold still as fields come and go
func sliderColumns(fields []*field) sliderCols {
	c := sliderCols{box: 4}
	for _, f := range fields {
		if f.kind != numberField {
			continue
		}
		c.box = max(c.box, ansi.StringWidth(f.format(f.hi)), ansi.StringWidth(f.format(f.lo)))
		c.lo = max(c.lo, ansi.StringWidth(f.mark(f.lo)))
		c.hi = max(c.hi, ansi.StringWidth(f.mark(f.hi)))
	}
	return c
}

// Reports whether an unfocused value shows in a faint well, which a theme that cannot blend never does
func (lk look) faint(t theme.Theme) bool { return !lk.focused && lk.framed && !t.Plain }

// Background of an input, lit while focused, faint while framed and bare otherwise
func (lk look) well(t theme.Theme) lipgloss.Style {
	switch {
	case lk.focused:
		return lipgloss.NewStyle().Background(t.Ghost)
	case lk.faint(t):
		return lipgloss.NewStyle().Background(theme.Mix(t.Surface, t.Ghost, wellTint))
	}
	return lipgloss.NewStyle()
}

// Rows of the value at width with its clicks in the look given, with the cursor column while typing
func (f *field) render(w int, lk look, cols sliderCols, t theme.Theme) ([]string, []hit, int) {
	switch f.kind {
	case choiceField:
		rows, hits := chips(f.choices, f.value, lk.focused, w, t)
		return rows, hits, -1
	case numberField:
		row, hits, x := f.slider(w, lk, cols, t)
		return []string{row}, hits, x
	}
	row, x := f.box(w, lk, t)
	return []string{row}, []hit{{row: 0, x0: 0, x1: w, choice: hitBox}}, x
}

// Choices as chips wrapped at width, the picked one lit, with the hits that map clicks back to choices
func chips(choices []string, picked string, focused bool, w int, t theme.Theme) ([]string, []hit) {
	var rows []string
	var hits []hit
	var sb strings.Builder
	x := 0
	for i, c := range choices {
		label := " " + c + " "
		lw := ansi.StringWidth(label)
		if x > 0 && x+1+lw > w {
			rows = append(rows, sb.String())
			sb.Reset()
			x = 0
		}
		if x > 0 {
			sb.WriteString(" ")
			x++
		}
		st := lipgloss.NewStyle().Foreground(t.TextDim)
		switch {
		case c == picked && focused:
			st = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Bold(true)
		case c == picked:
			st = lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true)
		case focused:
			st = lipgloss.NewStyle().Foreground(t.Text)
		}
		sb.WriteString(st.Render(label))
		hits = append(hits, hit{row: len(rows), x0: x, x1: x + lw, choice: i})
		x += lw
	}
	return append(rows, sb.String()), hits
}

// Text as an input well at width, masked when secret and scrolled to the cursor, with the cursor column
func (f *field) box(w int, lk look, t theme.Theme) (string, int) {
	base := lk.well(t)
	runes := []rune(f.value)
	if f.kind == secretField {
		runes = []rune(strings.Repeat(t.Glyphs.Mask, len(runes)))
	}
	if len(runes) == 0 {
		hint := f.placeholder
		if !lk.focused && !lk.faint(t) && hint == "" {
			hint = "-"
		}
		x := -1
		if lk.typing {
			x = 1
		}
		return base.Render(" ") + base.Foreground(t.TextDim).Render(render.Fit(hint, max(w-1, 0), t)), x
	}
	room := max(w-2, 1)
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

// Number well, range ends and slider on one row in shared columns, with their hits and the cursor column
func (f *field) slider(w int, lk look, cols sliderCols, t theme.Theme) (string, []hit, int) {
	lo, hi := f.mark(f.lo), f.mark(f.hi)
	inner := max(cols.box, ansi.StringWidth(f.format(f.hi)), ansi.StringWidth(f.format(f.lo)))
	loW, hiW := max(cols.lo, ansi.StringWidth(lo)), max(cols.hi, ansi.StringWidth(hi))
	bw := inner + 2
	base := lk.well(t)
	text := base.Foreground(t.Text).Render(render.Fit(f.value, inner, t))
	if f.value == "" {
		text = base.Foreground(t.TextDim).Render(render.Fit("-", inner, t))
	}
	box := base.Render(" ") + text + base.Render(" ")
	x := -1
	if lk.typing {
		x = 1 + min(f.cursor, inner)
	}
	hits := []hit{{row: 0, x0: 0, x1: bw, choice: hitBox}}
	tw := w - bw - 2 - loW - hiW - 2
	if tw < 6 || f.hi <= f.lo {
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

// Applies a click on a hit at column x, picking a choice, placing the slider or the cursor
func (f *field) click(h hit, x int) {
	switch {
	case h.choice >= 0 && h.choice < len(f.choices):
		if f.choices[h.choice] != f.value {
			f.set(f.choices[h.choice])
		}
	case h.choice == hitTrack:
		span := max(h.x1-h.x0-1, 1)
		f.setNumber(f.lo + float64(x-h.x0)/float64(span)*(f.hi-f.lo))
	case h.choice == hitBox && f.kind != numberField:
		f.cursor = min(max(x-h.x0-1, 0), len([]rune(f.value)))
	}
}

// Member of the list with the key, nil when none has it
func fieldOf(fields []*field, key string) *field {
	for _, f := range fields {
		if f.key == key {
			return f
		}
	}
	return nil
}
