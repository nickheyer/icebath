package cards

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Columns left of each label holding the bar that marks the focused field
const sheetGutter = 2

// Rows the mouse wheel scrolls by
const wheelRows = 3

// Clickable stretch of a sheet row or dialog foot, a field value or a button
type formHit struct {
	y, x0, x1 int
	field     *field
	hit       hit
	button    int
}

// Sheet laid out at one width, its rows with the clicks, field spans and cursor they place
type sheetLayout struct {
	lines []string
	hits  []formHit
	spans [][2]int
	cur   *tea.Cursor
}

// Column of labelled controls under section headings that dialogs and the settings tab share
type sheet struct {
	env    *Env
	fields []*field
	held   int
	at     int
	lastAt int
	typing bool
	bad    map[*field]string
	scroll int
	w      int
	body   int
	total  int
	// Laid out beside a scrollbar since the rows run past the view
	narrow bool
	spans  [][2]int
	hits   []formHit
	cur    *tea.Cursor
}

// Builds a sheet over the fields, focused on the first that shows
func newSheet(env *Env, fields []*field) *sheet {
	s := &sheet{env: env, fields: fields, lastAt: -1, held: -1, bad: map[*field]string{}}
	s.at = s.step(-1, 1)
	return s
}

// Builds the editor of every multi line field that has none yet
func (s *sheet) prepare() {
	for _, fd := range s.fields {
		if fd.kind == areaField && fd.area == nil {
			fd.area = newArea(s.env, fd.value)
		}
	}
}

// Hands the keys to the editor of the focused multi line field and takes them from every other
func (s *sheet) aim() {
	for i, fd := range s.fields {
		if fd.area == nil {
			continue
		}
		if i == s.at && fd.kind == areaField {
			fd.area.Focus()
		} else {
			fd.area.Blur()
		}
	}
}

// Index of the next shown field from i in the direction, i itself when none other shows
func (s *sheet) step(i, dir int) int {
	n := len(s.fields)
	for k := 1; k <= n; k++ {
		j := ((i+dir*k)%n + n) % n
		if s.fields[j].visible() {
			return j
		}
	}
	return max(i, 0)
}

// Field with the focus, nil when the sheet has none
func (s *sheet) focused() *field {
	if len(s.fields) == 0 {
		return nil
	}
	if s.at < 0 || s.at >= len(s.fields) || !s.fields[s.at].visible() {
		s.at = s.step(s.at, 1)
	}
	if s.at < 0 || s.at >= len(s.fields) || !s.fields[s.at].visible() {
		return nil
	}
	return s.fields[s.at]
}

// Scrolls the rows by wheel steps in the direction within the rows, leaving the focus where it is
func (s *sheet) wheel(dir int) {
	s.scroll = min(max(s.scroll+dir*wheelRows, 0), max(s.total-s.body, 0))
}

// Rows the help of the longest shown field takes at width, so the body holds still as the focus moves
func (s *sheet) helpRows(w int) int {
	room := 0
	for _, fd := range s.fields {
		if fd.visible() && fd.help != "" {
			room = max(room, strings.Count(render.Wrap(fd.help, max(w-sheetGutter, 1)), "\n")+1)
		}
	}
	return room
}

// Rows the whole sheet takes at width, every field over the help line
func (s *sheet) height(w int, t theme.Theme) int {
	n := len(s.layout(w, t).lines)
	if room := s.helpRows(w); room > 0 {
		n += room + 1
	}
	return n
}

// Rows of every shown field under its heading, a problem beneath the field it belongs to, laid out at width
func (s *sheet) layout(w int, t theme.Theme) sheetLayout {
	s.prepare()
	lw := 0
	for _, fd := range s.fields {
		if fd.visible() {
			lw = max(lw, ansi.StringWidth(fd.label))
		}
	}
	lw = min(lw, max(w/3, 8))
	left := sheetGutter + lw + 1
	vw := max(w-left, 8)
	cols := sliderColumns(s.fields)
	bar := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(t.Glyphs.Bar) + strings.Repeat(" ", sheetGutter-1)
	blank := strings.Repeat(" ", sheetGutter)
	out := sheetLayout{spans: make([][2]int, len(s.fields))}
	for i := range out.spans {
		out.spans[i] = [2]int{-1, -1}
	}
	group, first := "", true
	for i, fd := range s.fields {
		if !fd.visible() {
			continue
		}
		if fd.group != group {
			group = fd.group
			if !first {
				out.lines = append(out.lines, "")
			}
			if group != "" {
				out.lines = append(out.lines, blank+heading(group, w-sheetGutter, t))
			}
		} else if !first {
			out.lines = append(out.lines, "")
		}
		first = false
		focus := i == s.at
		rows, hits, cx, cy := fd.render(vw, look{focused: focus, typing: focus && s.typing}, cols, t)
		st, gutter := lipgloss.NewStyle().Foreground(t.TextDim), blank
		switch {
		case s.bad[fd] != "":
			st = lipgloss.NewStyle().Foreground(t.Alarm).Bold(focus)
		case focus:
			st = lipgloss.NewStyle().Foreground(t.Ice).Bold(true)
		}
		if focus {
			gutter = bar
		}
		top := len(out.lines)
		for r, row := range rows {
			label := ""
			if r == 0 {
				label = st.Render(render.Truncate(fd.label, lw, t))
			}
			out.lines = append(out.lines, gutter+render.Fit(label, lw, t)+" "+row)
		}
		for _, h := range hits {
			out.hits = append(out.hits, formHit{y: top + h.row, x0: left + h.x0, x1: left + h.x1, field: fd, hit: h})
		}
		if focus && cx >= 0 {
			out.cur = tea.NewCursor(left+cx, top+cy)
		}
		if p := s.bad[fd]; p != "" {
			for _, line := range strings.Split(render.Wrap(t.Glyphs.Fail+" "+p, vw), "\n") {
				out.lines = append(out.lines, gutter+render.Fit("", lw, t)+" "+render.Bad(line, t))
			}
		}
		out.spans[i] = [2]int{top, len(out.lines)}
	}
	return out
}

// Rows in view over the help of the focus, following the focus when it moved and the wheel otherwise
func (s *sheet) view(w, h int, t theme.Theme) string {
	s.prepare()
	s.aim()
	help := s.helpRows(w)
	room := max(h, 1)
	if help > 0 {
		room = max(h-help-1, 1)
	}
	lay := s.layout(w, t)
	s.narrow = len(lay.lines) > room && w >= minGutterWidth
	if s.narrow {
		lay = s.layout(w-2, t)
	}
	s.w, s.body, s.total = w, room, len(lay.lines)
	if fd := s.focused(); fd != nil && s.at != s.lastAt {
		if sp := lay.spans[s.at]; sp[0] >= 0 {
			if sp[1] > s.scroll+room {
				s.scroll = sp[1] - room
			}
			if sp[0] < s.scroll {
				s.scroll = sp[0]
			}
		}
	}
	s.lastAt = s.at
	s.scroll = min(max(s.scroll, 0), max(len(lay.lines)-room, 0))
	shown := lay.lines[min(s.scroll, len(lay.lines)):]
	shown = shown[:min(len(shown), room)]
	s.spans = lay.spans
	s.hits = s.hits[:0]
	for _, hh := range lay.hits {
		if hh.y < s.scroll || hh.y >= s.scroll+room {
			continue
		}
		hh.y -= s.scroll
		s.hits = append(s.hits, hh)
	}
	s.cur = lay.cur
	if s.cur != nil {
		s.cur.Y -= s.scroll
		if s.cur.Y < 0 || s.cur.Y >= room {
			s.cur = nil
		}
	}
	for len(shown) < room {
		shown = append(shown, "")
	}
	body := strings.Join(shown, "\n")
	if s.narrow {
		body = gutter{h: room, total: s.total, offset: s.scroll, visible: room}.beside(body, w, t)
	}
	if help == 0 {
		return body
	}
	lines := []string{""}
	if fd := s.focused(); fd != nil && fd.help != "" {
		for _, line := range strings.Split(render.Wrap(fd.help, max(w-sheetGutter, 1)), "\n") {
			lines = append(lines, strings.Repeat(" ", sheetGutter)+render.Dim(line, t))
		}
	}
	for len(lines) < help+1 {
		lines = append(lines, "")
	}
	return body + "\n" + strings.Join(lines, "\n")
}

func (s *sheet) onBar(x, y int) bool { return s.narrow && x >= s.w-2 && y >= 0 && y < s.body }
func (s *sheet) gutter() gutter {
	return gutter{h: s.body, total: s.total, offset: s.scroll, visible: s.body}
}
func (s *sheet) grabs(x, y int) bool { return s.onBar(x, y) && s.gutter().onTrack(y) }
func (s *sheet) page(x, y int) bool {
	if !s.onBar(x, y) {
		return false
	}
	g := s.gutter()
	switch kind, _ := g.hit(y); kind {
	case hitPageUp:
		s.scroll -= max(s.body-1, 1)
	case hitPageDown:
		s.scroll += max(s.body-1, 1)
	case hitJump:
		s.held = g.grab(y)
		s.scroll = g.dragTo(y, s.held)
	}
	return true
}

// Follows a drag of the thumb to row y, reporting whether one is going
func (s *sheet) drag(y int) bool {
	if s.held < 0 {
		return false
	}
	s.scroll = s.gutter().dragTo(y, s.held)
	return true
}

// Field under a click and the control hit, the hit nil when the click lands on the label
func (s *sheet) hitAt(x, y int) (int, *formHit) {
	if y < 0 || y >= s.body {
		return -1, nil
	}
	for i := range s.hits {
		h := &s.hits[i]
		if y == h.y && x >= h.x0 && x < h.x1 {
			return slices.Index(s.fields, h.field), h
		}
	}
	for i, sp := range s.spans {
		if sp[0] >= 0 && y+s.scroll >= sp[0] && y+s.scroll < sp[1] {
			return i, nil
		}
	}
	return -1, nil
}
