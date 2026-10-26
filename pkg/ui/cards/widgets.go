package cards

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Offset of the first line in view over a column of lines, kept inside the content
type scroll struct {
	offset int
	height int
	total  int
}

func (s *scroll) YOffset() int { return s.offset }
func (s *scroll) Height() int  { return s.height }

// Largest offset that still fills the view
func (s *scroll) maxOffset() int { return max(s.total-s.height, 0) }

func (s *scroll) SetYOffset(n int) { s.offset = min(max(n, 0), s.maxOffset()) }

// Sets the rows in view, keeping the offset inside the content
func (s *scroll) SetHeight(h int) {
	s.height = max(h, 0)
	s.SetYOffset(s.offset)
}

// Sets the lines of content, keeping the offset inside them
func (s *scroll) SetTotal(n int) {
	s.total = max(n, 0)
	s.SetYOffset(s.offset)
}

func (s *scroll) AtBottom() bool   { return s.offset >= s.maxOffset() }
func (s *scroll) GotoBottom()      { s.offset = s.maxOffset() }
func (s *scroll) GotoTop()         { s.offset = 0 }
func (s *scroll) ScrollDown(n int) { s.SetYOffset(s.offset + n) }
func (s *scroll) ScrollUp(n int)   { s.SetYOffset(s.offset - n) }
func (s *scroll) PageDown()        { s.ScrollDown(s.height) }
func (s *scroll) PageUp()          { s.ScrollUp(s.height) }

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

// Lines of the pane for row owner placed top rows down and rows tall, wrapped to fit beside the scrollbar
func (d *detailPane) view(lines []string, owner, top, rows, w int, env *Env) string {
	if owner != d.owner {
		d.scroll, d.owner = 0, owner
	}
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

// Pages or jumps for a click on the scrollbar, reporting whether the click landed in the pane
func (d *detailPane) click(x, y, w int, env *Env) bool {
	row := y - d.top
	if d.rows <= 0 || row < 0 || row >= d.rows {
		return false
	}
	if x == w-1 && w >= minGutterWidth {
		switch hit, line := d.gutter(env).hit(row); hit {
		case hitPageUp:
			d.by(-max(d.rows-1, 1))
		case hitPageDown:
			d.by(max(d.rows-1, 1))
		case hitJump:
			d.scroll = line
		}
	}
	return true
}

// Scrolls by n for a wheel at row y, reporting whether the wheel was over the pane
func (d *detailPane) wheel(y, n int) bool {
	if d.rows <= 0 || y < d.top || y >= d.top+d.rows {
		return false
	}
	d.by(n)
	return true
}

// Filter row with a glyph, the query, a cursor while it takes typing and a count at the right
type searchBar struct {
	query  string
	active bool
}

// Takes typing, keeping any query it already holds
func (b *searchBar) Open() { b.active = true }

// Keeps the query and stops taking typing
func (b *searchBar) Accept() { b.active = false }

// Drops the query and stops taking typing
func (b *searchBar) Close() {
	b.active = false
	b.query = ""
}

// Reports whether the bar takes typing
func (b *searchBar) Active() bool { return b.active }

// Reports whether the bar shows, while it takes typing or holds a query
func (b *searchBar) Shown() bool { return b.active || b.query != "" }

// Shows the bar taking typing, or drops it with its query when it already shows
func (b *searchBar) Toggle() {
	if b.Shown() {
		b.Close()
		return
	}
	b.Open()
}

// Keymap contexts while the bar takes typing, the toggle key closing it again until the first character
func (b *searchBar) Contexts() []string {
	if b.query == "" {
		return []string{keys.Search, keys.Filter}
	}
	return []string{keys.Search}
}

// Text searched for
func (b *searchBar) Query() string { return b.query }

// Carries out a search editing action, reporting whether the query changed
func (b *searchBar) Action(id string) bool {
	switch id {
	case "search.delete.back":
		if r := []rune(b.query); len(r) > 0 {
			b.query = string(r[:len(r)-1])
			return true
		}
	case "search.clear":
		if b.query != "" {
			b.query = ""
			return true
		}
	case "search.delete.word":
		r := []rune(b.query)
		i := len(r)
		for i > 0 && r[i-1] == ' ' {
			i--
		}
		for i > 0 && r[i-1] != ' ' {
			i--
		}
		if i != len(r) {
			b.query = string(r[:i])
			return true
		}
	}
	return false
}

// Appends typed text to the query, line breaks folded into spaces
func (b *searchBar) Type(text string) {
	b.query += strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text)
}

// Column the terminal cursor sits at inside the bar
func (b *searchBar) CursorX() int { return 3 + ansi.StringWidth(b.query) }

// Row at width with the count at the right, lit as a field while it takes typing
func (b *searchBar) View(w int, count string, placeholder string, t theme.Theme) string {
	glyph := t.Glyphs.Search
	base := lipgloss.NewStyle()
	if b.active {
		base = base.Background(t.Ghost)
	}
	text := base.Foreground(t.Text).Render(b.query)
	if b.query == "" {
		text = base.Foreground(t.TextDim).Render(placeholder)
	}
	mark := t.TextDim
	if b.active || b.query != "" {
		mark = t.Accent
	}
	left := base.Foreground(mark).Render(" "+glyph+" ") + text
	right := ""
	if count != "" {
		right = base.Foreground(t.TextDim).Render(count + " ")
	}
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw+rw+1 > w {
		return render.Fit(left, w, t)
	}
	return left + base.Render(strings.Repeat(" ", w-lw-rw)) + right
}

// Self rescheduling timer of one card whose ticks no other card and no abandoned loop acts on
type ticker struct {
	every time.Duration
	gen   int
	armed bool
}

// Tick a ticker scheduled, naming the ticker and the loop it belongs to
type tickMsg struct {
	owner *ticker
	gen   int
}

// Schedules the next tick unless one is already pending
func (k *ticker) start() tea.Cmd {
	if k.armed {
		return nil
	}
	k.gen++
	k.armed = true
	owner, gen := k, k.gen
	return tea.Tick(k.every, func(time.Time) tea.Msg { return tickMsg{owner: owner, gen: gen} })
}

// Abandons the pending tick so the next start begins a fresh loop
func (k *ticker) reset() { k.armed = false }

// Reports whether the message is the pending tick, disarming it so the owner may schedule the next
func (k *ticker) owns(msg tickMsg) bool {
	if msg.owner != k || msg.gen != k.gen || !k.armed {
		return false
	}
	k.armed = false
	return true
}
