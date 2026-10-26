package cards

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Width a dialog asks for, the overlay narrowing it on a small screen
const formWidth = 88

// Columns left of each label holding the bar that marks the focused field
const formGutter = 2

// Rows a multi line field shows, its editor scrolling past them
const areaRows = 8

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

func (f *Form) Title() string       { return f.title }
func (f *Form) Glyph() string       { return f.glyph }
func (f *Form) Key() string         { return "form:" + f.title }
func (f *Form) Init() tea.Cmd       { return nil }
func (f *Form) Cursor() *tea.Cursor { return f.cur }

// Dialog keys, under the editor keys while a multi line field has the focus
func (f *Form) Contexts() []string {
	if fd := f.focused(); fd != nil && fd.kind == areaField {
		return []string{keys.Area, keys.Form}
	}
	return []string{keys.Form}
}

// Builds the editor of every multi line field that has none yet
func (f *Form) prepare() {
	for _, fd := range f.fields {
		if fd.kind == areaField && fd.area == nil {
			fd.area = newArea(f.env, fd.value)
		}
	}
}

// Hands the keys to the editor of the focused multi line field and takes them from every other
func (f *Form) aim() {
	for i, fd := range f.fields {
		if fd.area == nil {
			continue
		}
		if i == f.at && fd.kind == areaField {
			fd.area.Focus()
		} else {
			fd.area.Blur()
		}
	}
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

// Hands a key or paste to the editor of a multi line field and takes its text back as the value
func (f *Form) typeArea(fd *field, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	*fd.area, cmd = fd.area.Update(msg)
	fd.value = fd.area.Value()
	fd.cursor = len([]rune(fd.value))
	delete(f.bad, fd)
	return cmd
}

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
	f.prepare()
	fd := f.focused()
	f.aim()
	switch msg := msg.(type) {
	case wm.ActionMsg:
		return f, f.act(msg)
	case tea.KeyPressMsg:
		if fd != nil && fd.kind == areaField {
			return f, f.typeArea(fd, msg)
		}
		if fd != nil && msg.Text != "" {
			fd.typeText(msg.Text)
			delete(f.bad, fd)
		}
	case tea.PasteMsg:
		if fd != nil && fd.kind == areaField {
			return f, f.typeArea(fd, msg)
		}
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
func (f *Form) act(msg wm.ActionMsg) tea.Cmd {
	fd := f.focused()
	id := msg.ID
	if fd != nil && fd.kind == areaField {
		if cmd, taken := f.actArea(fd, id); taken {
			return cmd
		}
	}
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

// Carries out an action on the editor of the focused multi line field, reporting whether it was one for it
func (f *Form) actArea(fd *field, id string) (tea.Cmd, bool) {
	a := fd.area
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
			f.at = f.step(f.at, -1)
			return nil, true
		}
		a.CursorUp()
	case "area.down":
		if li := a.LineInfo(); a.Line() == a.LineCount()-1 && li.RowOffset >= li.Height-1 {
			f.at = f.step(f.at, 1)
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
	delete(f.bad, fd)
	return nil, true
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
	f.prepare()
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
		i      int
		fd     *field
		lines  []string
		hits   []hit
		cx, cy int
	}
	// Rows of one field in the look given, its problem wrapped under the value
	paint := func(i int, fd *field, framed bool) part {
		focus := i == f.at
		rows, hits, cx, cy := fd.render(vw, look{focused: focus, typing: focus, framed: framed}, cols, t)
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
				label = st.Render(render.Truncate(fd.label, lw-2, t))
			}
			lines = append(lines, render.Fit(label, lw, t)+row)
		}
		if p := f.bad[fd]; p != "" {
			for _, line := range strings.Split(render.Wrap(p, vw), "\n") {
				lines = append(lines, render.Fit("", lw, t)+render.Bad(line, t))
			}
		}
		return part{i: i, fd: fd, lines: lines, hits: hits, cx: cx, cy: cy}
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
	bar := t.Glyphs.Bar
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
			out.cur = tea.NewCursor(left+p.cx, top+p.cy)
		}
		out.spans[p.i] = [2]int{top, len(out.body)}
	}
	out.hits = append(out.hits, buttons...)
	return out
}

// Rule, help of the focus in room for the longest help, any save error, and buttons with their clicks
func (f *Form) footer(w int) ([]string, []formHit) {
	t := f.env.Theme
	foot := []string{lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, w))}
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
	f.prepare()
	f.aim()
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

// Actions the dialog answers, choosing keys on a choice or number, editor keys on a multi line field
func (f *Form) Hints() []wm.Hint {
	out := []wm.Hint{{Action: "form.submit"}, {Action: "form.cancel"}, {Action: "form.next", Pair: "form.prev", Label: "field"}}
	if fd := f.focused(); fd != nil {
		if fd.kind == areaField {
			out = []wm.Hint{{Action: "area.save"}, {Action: "form.cancel"}, {Action: "area.newline"}, {Action: "form.next", Pair: "form.prev", Label: "field"}}
		}
		if fd.kind == choiceField || fd.kind == numberField {
			out = append(out, wm.Hint{Action: "form.left", Pair: "form.right", Label: "change"})
		}
		if fd.optional {
			out = append(out, wm.Hint{Action: "form.unset"})
		}
	}
	return out
}

// Kind of value a field takes
type fieldKind int

const (
	textField fieldKind = iota
	secretField
	choiceField
	numberField
	areaField
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
	// Editor of a multi line field, built by the dialog on first use
	area *textarea.Model
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

// Rows of the value at width with its clicks in the look given, and the cursor cell while typing
func (f *field) render(w int, lk look, cols sliderCols, t theme.Theme) ([]string, []hit, int, int) {
	switch f.kind {
	case choiceField:
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
	a.Placeholder = f.placeholder
	if !lk.focused && !lk.faint(t) && a.Placeholder == "" {
		a.Placeholder = "-"
	}
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
	runes := []rune(strings.Map(oneLine, f.value))
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

// Line break shown as a space so a value holding several lines fits its one row
func oneLine(r rune) rune {
	if r == '\n' || r == '\r' {
		return ' '
	}
	return r
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
	case h.choice == hitBox && f.kind == areaField:
		f.placeCursor(h.row, x-h.x0-1)
	case h.choice == hitBox && f.kind != numberField:
		f.cursor = min(max(x-h.x0-1, 0), len([]rune(f.value)))
	}
}
