package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Scrollbar beside scrolling content, its caps showing the paging keys and its track a timeline of turns and matches
type gutter struct {
	h       int
	total   int
	offset  int
	visible int
	// Lines that start a turn, ticked along the track
	marks []int
	// Lines holding a search match
	hits []int
	// Chords that page up and down
	up   string
	down string
}

// What a click on the gutter asks for
type gutterHit int

const (
	hitNone gutterHit = iota
	hitPageUp
	hitPageDown
	hitJump
)

// First row of the track and its length, between the two caps
func (g gutter) track() (start, length int) {
	return 1, max(g.h-2, 0)
}

// Reports whether the content runs past the rows in view
func (g gutter) scrolls() bool { return g.total > g.visible && g.total > 0 }

// Rows the thumb covers within the track, none while nothing scrolls
func (g gutter) thumb() (start, length int) {
	ts, tl := g.track()
	if tl <= 0 || !g.scrolls() {
		return ts, 0
	}
	length = min(max(g.visible*tl/g.total, 1), tl)
	start = ts + g.offset*(tl-length)/max(g.total-g.visible, 1)
	return start, length
}

// Reports whether row y lies on the track between the caps while the content scrolls
func (g gutter) onTrack(y int) bool {
	ts, tl := g.track()
	return g.scrolls() && y >= ts && y < ts+tl
}

// Row of the thumb a press on row y took hold of, its middle for a press on the track beside it
func (g gutter) grab(y int) int {
	th, thl := g.thumb()
	if y >= th && y < th+thl {
		return y - th
	}
	return thl / 2
}

// Offset that puts the held row of the thumb under row y of the track
func (g gutter) dragTo(y, held int) int {
	ts, tl := g.track()
	_, thl := g.thumb()
	room := tl - thl
	if room <= 0 || !g.scrolls() {
		return g.offset
	}
	start := min(max(y-held-ts, 0), room)
	return start * (g.total - g.visible) / room
}

// Track row a content line falls on, beside the line itself while nothing scrolls
func (g gutter) rowOf(line int) int {
	ts, tl := g.track()
	if !g.scrolls() {
		return min(max(line, ts), ts+tl-1)
	}
	return ts + line*tl/g.total
}

// Cap of a paging chord, the theme symbol of its key or an arrow with the chord spelled beside it
func capOf(chord, arrow string, t theme.Theme) (glyph, label string) {
	if sym, ok := t.Glyphs.Chords[chord]; ok {
		return sym, ""
	}
	return arrow, keys.Format(chord)
}

// Column of h cells, one per row, blank while everything fits in view
func (g gutter) column(t theme.Theme) []string {
	if g.h <= 0 {
		return nil
	}
	if !g.scrolls() {
		out := make([]string, g.h)
		for i := range out {
			out[i] = " "
		}
		return out
	}
	gl := t.Glyphs
	upArrow, downArrow, line, heavy, tick, heavyTick, hit := gl.Up, gl.Down, gl.Track, gl.Heavy, gl.Tick, gl.HeavyTick, gl.Hit
	capStyle := lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
	out := make([]string, g.h)
	up, _ := capOf(g.up, upArrow, t)
	down, _ := capOf(g.down, downArrow, t)
	out[0] = capStyle.Render(up)
	if g.h == 1 {
		return out
	}
	out[g.h-1] = capStyle.Render(down)
	ts, tl := g.track()
	th, thl := g.thumb()
	kinds := make([]int, g.h)
	if tl > 0 && g.total > 0 {
		for _, l := range g.marks {
			if row := g.rowOf(l); row >= ts && row < ts+tl {
				kinds[row] = max(kinds[row], 1)
			}
		}
		for _, l := range g.hits {
			if row := g.rowOf(l); row >= ts && row < ts+tl {
				kinds[row] = 2
			}
		}
	}
	for row := ts; row < ts+tl; row++ {
		onThumb := row >= th && row < th+thl
		st, glyph := lipgloss.NewStyle().Foreground(t.Steel), line
		if onThumb {
			st, glyph = lipgloss.NewStyle().Foreground(t.Ice), heavy
		}
		switch kinds[row] {
		case 1:
			glyph = tick
			if onThumb {
				glyph = heavyTick
			} else {
				st = st.Foreground(t.SteelBright)
			}
		case 2:
			st, glyph = lipgloss.NewStyle().Foreground(t.Accent).Bold(true), hit
		}
		out[row] = st.Render(glyph)
	}
	return out
}

// Meaning of a click on row y, the line to show first for a jump
func (g gutter) hit(y int) (gutterHit, int) {
	if y < 0 || y >= g.h || !g.scrolls() {
		return hitNone, 0
	}
	ts, tl := g.track()
	switch {
	case y == 0:
		return hitPageUp, 0
	case y == g.h-1:
		return hitPageDown, 0
	case tl <= 0:
		return hitNone, 0
	}
	line := (y - ts) * g.total / tl
	return hitJump, min(max(line-g.visible/2, 0), max(g.total-g.visible, 0))
}

// Body fitted to two columns short of w with the gutter in the last column, a chord without a symbol set beside its cap
func (g gutter) beside(body string, w int, t theme.Theme) string {
	col := g.column(t)
	rows := strings.Split(body, "\n")
	for len(rows) < g.h {
		rows = append(rows, "")
	}
	upLabel, downLabel := "", ""
	if g.scrolls() {
		_, upLabel = capOf(g.up, "", t)
		_, downLabel = capOf(g.down, "", t)
	}
	label := lipgloss.NewStyle().Foreground(t.TextDim)
	for i := range rows[:g.h] {
		text := ""
		switch {
		case i == 0:
			text = upLabel
		case i == g.h-1:
			text = downLabel
		}
		if text == "" || ansi.StringWidth(text)+4 > w {
			rows[i] = render.Fit(rows[i], w-2, t) + " " + col[i]
			continue
		}
		lw := ansi.StringWidth(text)
		rows[i] = render.Fit(rows[i], w-3-lw, t) + " " + label.Render(text) + " " + col[i]
	}
	return strings.Join(rows[:g.h], "\n")
}

// Wrapped lines under a table that scroll on their own beside a scrollbar capped with the detail keys
type detailPane struct {
	scroll int
	owner  int
	top    int
	rows   int
	total  int
	// One past the row of the thumb held through a drag, zero between drags
	held int
}

// Moves the pane by n lines, kept inside its lines
func (d *detailPane) by(n int) {
	d.scroll = min(max(d.scroll+n, 0), max(d.total-d.rows, 0))
}

// Takes no rows, for a view that shows no detail
func (d *detailPane) hide() { d.rows, d.total = 0, 0 }

// Reports whether the pane holds more lines than it shows
func (d *detailPane) overflows() bool { return d.total > d.rows }

// Scrollbar of the pane capped with the keys that page it
func (d *detailPane) gutter(env *Env) gutter {
	return gutter{h: d.rows, total: d.total, offset: d.scroll, visible: d.rows, up: env.Keys.First("detail.scroll.up"), down: env.Keys.First("detail.scroll.down")}
}

// Lines of a detail wrapped to fit the pane at width w beside its scrollbar
func (d *detailPane) wrap(lines []string, w int) []string {
	bw := w
	if w >= minGutterWidth {
		bw = w - 2
	}
	var body []string
	for _, line := range lines {
		for _, part := range strings.Split(render.Wrap(line, max(bw-2, 1)), "\n") {
			body = append(body, "  "+part)
		}
	}
	return body
}

// Lines of the pane for row owner placed top rows down and rows tall, wrapped to fit beside the scrollbar
func (d *detailPane) view(lines []string, owner, top, rows, w int, env *Env) string {
	if owner != d.owner {
		d.scroll, d.owner = 0, owner
	}
	bw := w
	if w >= minGutterWidth {
		bw = w - 2
	}
	body := d.wrap(lines, w)
	d.top, d.rows, d.total = top, max(rows, 0), len(body)
	d.by(0)
	shown := body[min(d.scroll, len(body)):]
	shown = shown[:min(len(shown), d.rows)]
	text := strings.Join(shown, "\n")
	if bw < w && d.rows > 0 {
		text = d.gutter(env).beside(text, w, env.Theme)
	}
	return text
}

// Carries out a detail scroll action, reporting whether it was one
func (d *detailPane) act(id string) bool {
	switch id {
	case "detail.scroll.down":
		d.by(max(d.rows-1, 1))
	case "detail.scroll.up":
		d.by(-max(d.rows-1, 1))
	default:
		return false
	}
	return true
}

// Reports whether the cell lies on the scrollbar of the pane
func (d *detailPane) onBar(x, y, w int) bool {
	row := y - d.top
	return d.rows > 0 && row >= 0 && row < d.rows && w >= minGutterWidth && x >= w-2
}

// Reports whether a press at the cell takes hold of the scrollbar
func (d *detailPane) grabs(x, y, w int, env *Env) bool {
	return d.onBar(x, y, w) && d.gutter(env).onTrack(y-d.top)
}

// Pages for a click on a cap, takes hold of the thumb for one on the track, reporting whether the click landed in the pane
func (d *detailPane) click(x, y, w int, env *Env) bool {
	row := y - d.top
	if d.rows <= 0 || row < 0 || row >= d.rows {
		return false
	}
	if d.onBar(x, y, w) {
		g := d.gutter(env)
		switch hit, _ := g.hit(row); hit {
		case hitPageUp:
			d.by(-max(d.rows-1, 1))
		case hitPageDown:
			d.by(max(d.rows-1, 1))
		case hitJump:
			d.held = g.grab(row) + 1
			d.scroll = g.dragTo(row, d.held-1)
		}
	}
	return true
}

// Follows a drag of the thumb to row y, reporting whether one is going
func (d *detailPane) drag(y int, env *Env) bool {
	if d.held <= 0 {
		return false
	}
	d.scroll = d.gutter(env).dragTo(y-d.top, d.held-1)
	return true
}

// Lets go of the thumb
func (d *detailPane) release() { d.held = 0 }

// Scrolls by n for a wheel at row y, reporting whether the wheel was over the pane
func (d *detailPane) wheel(y, n int) bool {
	if d.rows <= 0 || y < d.top || y >= d.top+d.rows {
		return false
	}
	d.by(n)
	return true
}

// Filter row with a glyph, the query edited with the keys every field shares, a cursor while it takes typing and a count at the right
type searchBar struct {
	in     *field.Field
	active bool
	// Always takes typing once opened, as the bar of a list to pick from does
	pinned bool
	// Columns the query had in the last view, where its caret was placed
	room int
}

// Field of the bar, made on first use
func (b *searchBar) input() *field.Field {
	if b.in == nil {
		b.in = field.New("")
	}
	return b.in
}

// Takes typing, keeping any query it already holds
func (b *searchBar) Open() { b.active = true }

// Keeps the query and stops taking typing, a pinned bar going on taking it
func (b *searchBar) Accept() { b.active = b.active && b.pinned }

// Drops the query and stops taking typing, a pinned bar going on taking it
func (b *searchBar) Close() {
	b.active = b.active && b.pinned
	b.in = nil
}

// Reports whether the bar takes typing
func (b *searchBar) Active() bool { return b.active }

// Reports whether the bar shows, while it takes typing or holds a query
func (b *searchBar) Shown() bool { return b.active || b.Query() != "" }

// Shows the bar taking typing, or drops it with its query when it already shows
func (b *searchBar) Toggle() {
	if b.Shown() {
		b.Close()
		return
	}
	b.Open()
}

// Keymap contexts while the bar takes typing, the toggle key closing it again until the first character, the field keys last
func (b *searchBar) Contexts() []string {
	if b.Query() == "" && !b.pinned {
		return []string{keys.Search, keys.Filter, keys.Text}
	}
	return []string{keys.Search, keys.Text}
}

// Text searched for
func (b *searchBar) Query() string {
	if b.in == nil {
		return ""
	}
	return b.in.Value()
}

// Carries out an editing action on the query, reporting whether the query changed, with the command it needs
func (b *searchBar) Action(id string) (bool, tea.Cmd) {
	before := b.Query()
	cmd, _ := b.input().Action(id)
	return b.Query() != before, cmd
}

// Takes typed or pasted text into the query, reporting whether it changed
func (b *searchBar) Take(msg tea.Msg) bool { return b.input().Take(msg) }

// Column the terminal cursor sits at inside the bar
func (b *searchBar) CursorX() int {
	_, caret := b.input().Line(max(b.room, 1))
	return 3 + caret
}

// Row at width with the count at the right, lit as a field while it takes typing
func (b *searchBar) View(w int, count string, placeholder string, t theme.Theme) string {
	glyph := t.Glyphs.Search
	base := lipgloss.NewStyle()
	if b.active {
		base = base.Background(t.Ghost)
	}
	right := ""
	if count != "" {
		right = base.Foreground(t.TextDim).Render(count + " ")
	}
	b.room = max(w-3-ansi.StringWidth(right)-1, 1)
	query, _ := b.input().Line(b.room)
	text := base.Foreground(t.Text).Render(query)
	if b.Query() == "" {
		text = base.Foreground(t.TextDim).Render(placeholder)
	}
	mark := t.TextDim
	if b.active || b.Query() != "" {
		mark = t.Accent
	}
	left := base.Foreground(mark).Render(" "+glyph+" ") + text
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw+rw+1 > w {
		return render.Fit(left, w, t)
	}
	return left + base.Render(strings.Repeat(" ", w-lw-rw)) + right
}

// Puts text into the query as if typed, line breaks folded into spaces
func (b *searchBar) Type(text string) { b.input().Insert(text) }
