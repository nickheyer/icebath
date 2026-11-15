package arena

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Scrollable sheet of everything the harness reports of one member, the body of the inspector and of a member window
type sheet struct {
	a           *arena
	name        string
	top         int
	rows, shown int
}

// Scrolls the sheet by a count of rows, kept inside its lines
func (s *sheet) scrollBy(n int) {
	s.top = min(max(s.top+n, 0), max(s.rows-s.shown, 0))
}

// Scrolls the sheet with the wheel, the rest of the pointer passing by
func (s *sheet) Update(msg tea.Msg) (*sheet, tea.Cmd) {
	if m, ok := msg.(tea.MouseWheelMsg); ok {
		switch m.Button {
		case tea.MouseWheelUp:
			s.scrollBy(-3)
		case tea.MouseWheelDown:
			s.scrollBy(3)
		}
	}
	return s, nil
}

// Lines of the sheet from its scroll, wrapped to the width and cut to the height
func (s *sheet) View(w, h int) string {
	lines := s.lines(w)
	s.rows, s.shown = len(lines), h
	s.top = min(s.top, max(len(lines)-h, 0))
	end := min(s.top+h, len(lines))
	return strings.Join(lines[s.top:end], "\n")
}

// Every line of what the harness reports of the member, headed by kind
func (s *sheet) lines(w int) []string {
	a := s.a
	t := a.env.Theme
	m, ok := a.snap.Member(s.name)
	if !ok || w < 8 {
		return nil
	}
	live := a.live[m.Name]
	var out []string
	head := func(title string) {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(strings.ToUpper(title)))
	}
	text := func(s string) {
		out = append(out, cards.WrapStyled(s, w, t.Text)...)
	}
	dim := func(s string) {
		for _, l := range cards.WrapIndent(s, w, 2) {
			out = append(out, render.Dim(l, t))
		}
	}
	if m.Role == RoleDirector {
		head("prompt")
	} else {
		head("goal")
	}
	if m.Task != "" {
		text(m.Task)
	} else {
		dim("none, the agent works without a prompt of this turn")
	}
	head("status")
	state := m.Status
	if m.Depth > 0 {
		state += fmt.Sprintf(", depth %d", m.Depth)
	}
	if m.Parent != "" {
		state += ", delegated by " + m.Parent
	}
	text(state)
	if !m.Started.IsZero() {
		took := m.Duration
		if took == 0 {
			took = a.moment().Sub(m.Started)
		}
		dim("started " + m.Started.Local().Format("15:04:05") + ", " + render.Elapsed(took.Truncate(time.Second)))
	}
	if m.Model != "" {
		dim("model " + m.Model)
	}
	if m.Ticket != "" {
		if tk, ok := a.snap.Ticket(m.Ticket); ok {
			dim(fmt.Sprintf("ticket %s %s", tk.ID, tk.Status))
		}
	}
	head("tools")
	if live.tool != "" && live.running {
		text(render.ToolGlyph(live.tool, t) + " " + live.tool + " now")
	}
	tl := live.tally
	if tl.total() == 0 {
		dim("no tool started yet")
	} else {
		text(fmt.Sprintf("%d calls", tl.total()))
		var parts []string
		for _, c := range []struct {
			n    int
			name string
		}{{tl.reads, "read"}, {tl.edits, "edit"}, {tl.shells, "shell"}, {tl.fetches, "web"}, {tl.agents, "delegate"}, {tl.calls, "harness"}, {tl.others, "other"}} {
			if c.n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
			}
		}
		dim(strings.Join(parts, " · "))
	}
	if u := a.snap.Usage[m.Name]; u.Input+u.Output+u.Reasoning > 0 || u.Cost > 0 || live.contextLimit > 0 {
		head("spend")
		text(fmt.Sprintf("%s in, %s out, %s reasoning", render.Tokens(u.Input), render.Tokens(u.Output), render.Tokens(u.Reasoning)))
		if u.Cost > 0 {
			text(render.Money(u.Cost))
		}
		if live.contextLimit > 0 {
			dim(fmt.Sprintf("context %s of %s, %s", render.Tokens(live.context), render.Tokens(live.contextLimit), render.Percent(float64(live.context)/float64(live.contextLimit))))
		}
	}
	if m.Thinking != "" {
		head("thinking")
		dim(m.Thinking)
	}
	if len(m.Notes) > 0 {
		head("notes")
		for _, n := range m.Notes {
			dim(n)
		}
	}
	if m.Summary != "" {
		head("summary")
		text(m.Summary)
	}
	for _, files := range []struct {
		title string
		list  []string
	}{{"files read", m.FilesRead}, {"files written", m.FilesWritten}} {
		if len(files.list) == 0 {
			continue
		}
		head(fmt.Sprintf("%s %d", files.title, len(files.list)))
		for _, f := range files.list {
			if fw := ansi.StringWidth(f); fw > w {
				f = ansi.TruncateLeft(f, fw-w+1, t.Glyphs.Ellipsis)
			}
			out = append(out, render.Dim(f, t))
		}
	}
	if len(m.Output) > 0 {
		head("output")
		for _, o := range m.Output {
			dim(o)
		}
	}
	var kids []string
	for _, o := range a.snap.Members {
		if o.Parent == m.Name && o.Name != m.Name {
			kids = append(kids, o.Name)
		}
	}
	if len(kids) > 0 {
		head("delegated to")
		text(strings.Join(kids, ", "))
	}
	if len(m.Toolsets) > 0 {
		head("toolsets")
		dim(strings.Join(m.Toolsets, ", "))
	}
	return out
}

// Member window of the world off display, the member it shows and its box
type tile struct {
	name string
	r    rect
}

// Rows of a member window at the least
const tileRows = 6

// Member windows over the world box, each a header naming the member and its sheet under it
func (a *arena) paintTiles(p *cards.Painter) {
	t := a.env.Theme
	wb := a.lay.world
	a.tiles = a.tiles[:0]
	p.Fill(wb.x, wb.y, wb.w, wb.h, t.Bg)
	if a.snap == nil {
		msg := "the world is off, member windows show here once a turn runs"
		p.Label(wb.x+max((wb.w-ansi.StringWidth(msg))/2, 0), wb.y+wb.h/2, msg, t.TextDim, t.Bg, false)
		return
	}
	n := len(a.snap.Members)
	cols := min(max(wb.w/44, 1), n)
	rows := (n + cols - 1) / cols
	for rows > 1 && wb.h/rows < tileRows {
		rows--
	}
	cw, ch := wb.w/cols, wb.h/max(rows, 1)
	for i, m := range a.snap.Members {
		if i >= cols*rows {
			break
		}
		r := rect{wb.x + (i%cols)*cw, wb.y + (i/cols)*ch, cw, ch}
		if i%cols == cols-1 {
			r.w = wb.x + wb.w - r.x
		}
		a.tiles = append(a.tiles, tile{name: m.Name, r: r})
		bg := t.Surface
		if m.Name == a.sel {
			bg = theme.Mix(t.Surface, t.Ice, 0.12)
		}
		p.Fill(r.x, r.y, r.w-1, r.h, bg)
		head := " " + a.memberMark(m) + " " + lipgloss.NewStyle().Foreground(t.Text).Bold(true).Render(m.Name) + " " + render.Dim(string(m.Role), t)
		p.Styled(r.x, r.y, r.w-1, 1, ansi.Truncate(head, r.w-1, t.Glyphs.Ellipsis))
		a.hits = append(a.hits, hit{id: "tile:" + m.Name, r: rect{r.x, r.y, r.w - 1, 1}})
		if s := a.sessionOf(m.Name); s != nil && r.h > 1 {
			p.Styled(r.x+1, r.y+1, r.w-3, r.h-1, s.View(r.w-3, r.h-1))
		}
	}
}

// Member window under a cell of the last paint
func (a *arena) tileAt(x, y int) (tile, bool) {
	for _, tl := range a.tiles {
		if tl.r.contains(x, y) {
			return tl, true
		}
	}
	return tile{}, false
}
