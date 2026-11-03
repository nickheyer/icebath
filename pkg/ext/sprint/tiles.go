package sprint

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/ui/cards"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Session window of one member while no world shows, its box with the header row on top
type tile struct {
	name string
	r    rect
}

// Fewest columns a session window takes and fewest rows under its header
const (
	minTileWidth  = 44
	minTileHeight = 6
)

// Lays the session windows of the team over the floor box in a grid, as many across as fit
func (a *arena) arrangeTiles() []tile {
	members := a.roster()
	wb := a.lay.world
	n := len(members)
	if n == 0 || wb.w < 10 || wb.h < 3 {
		return nil
	}
	cols := max(min(n, wb.w/minTileWidth), 1)
	rows := (n + cols - 1) / cols
	for rows*minTileHeight > wb.h && cols < n {
		cols++
		rows = (n + cols - 1) / cols
	}
	tw, th := wb.w/cols, wb.h/rows
	out := make([]tile, 0, n)
	for i, m := range members {
		c, r := i%cols, i/cols
		x, y := wb.x+c*tw, wb.y+r*th
		w, h := tw, th
		if c == cols-1 {
			w = wb.x + wb.w - x
		}
		if r == rows-1 {
			h = wb.y + wb.h - y
		}
		out = append(out, tile{name: m.Name, r: rect{x, y, w, h}})
	}
	return out
}

// Paints every session window of the team side by side, the selected one marked and the typed to one lit
func (a *arena) paintTiles(p *cards.Painter) {
	t := a.env.Theme
	wb := a.lay.world
	p.Fill(wb.x, wb.y, wb.w, wb.h, t.Bg)
	a.tiles = a.arrangeTiles()
	if len(a.tiles) == 0 {
		msg := "no sprint yet, run /" + Name + " <request> to start one"
		if a.snap != nil {
			msg = "nobody is on the team yet"
		}
		p.Label(wb.x+max((wb.w-ansi.StringWidth(msg))/2, 0), wb.y+wb.h/2, msg, t.TextDim, t.Bg, false)
		return
	}
	for _, tl := range a.tiles {
		m, ok := a.snap.Member(tl.name)
		if !ok {
			continue
		}
		gap := 0
		if tl.r.x+tl.r.w < wb.x+wb.w {
			gap = 1
		}
		w, h := tl.r.w-gap, tl.r.h
		p.Fill(tl.r.x, tl.r.y, w, h, t.Surface)
		live := a.live[m.Name]
		glyph, fg := a.stateMark(live)
		bg := theme.Mix(t.Surface, t.Steel, 0.7)
		nameFg, dimFg := t.Text, t.TextDim
		switch {
		case m.Name == a.sel && a.typing:
			bg, nameFg, dimFg, fg = t.Ice, t.Bg, theme.Mix(t.Bg, t.Ice, 0.35), t.Bg
		case m.Name == a.sel:
			bg = theme.Mix(t.Surface, t.Ice, 0.3)
		case m.Name == a.hover || a.hoverHit == "tile:"+m.Name:
			bg = t.SteelBright
		}
		p.Fill(tl.r.x, tl.r.y, w, 1, bg)
		x := tl.r.x + 1
		x += p.Label(x, tl.r.y, glyph+" ", fg, bg, false)
		x += p.Label(x, tl.r.y, m.Name, nameFg, bg, true)
		if role := string(m.Role); !strings.EqualFold(role, m.Name) && !strings.HasPrefix(strings.ToLower(m.Name), role) {
			x += p.Label(x, tl.r.y, " "+role, dimFg, bg, false)
		}
		if m.Ticket != "" {
			x += p.Label(x, tl.r.y, " "+m.Ticket, dimFg, bg, false)
		}
		if live.running && live.tool != "" && x+len(live.tool)+2 < tl.r.x+w {
			x += p.Label(x, tl.r.y, " "+live.tool, dimFg, bg, false)
		}
		if m.Live != nil && m.Live.Endpoint.Model != "" {
			model := m.Live.Endpoint.Model
			if mx := tl.r.x + w - 1 - ansi.StringWidth(model); mx > x+1 {
				p.Label(mx, tl.r.y, model, dimFg, bg, false)
			}
		}
		a.hits = append(a.hits, hit{id: "tile:" + m.Name, r: rect{tl.r.x, tl.r.y, w, 1}})
		if h < 2 {
			continue
		}
		s := a.session(m)
		if s == nil {
			lines := cards.WrapIndent(render.Dim(m.Name+" has no session yet", t), w-2, 0)
			p.Lines(tl.r.x+1, tl.r.y+1, w-2, h-1, lines)
			continue
		}
		p.Styled(tl.r.x, tl.r.y+1, w, h-1, s.View(w, h-1))
	}
}

// Session window holding a cell of the last paint
func (a *arena) tileAt(x, y int) (tile, bool) {
	for _, tl := range a.tiles {
		if tl.r.contains(x, y) {
			return tl, true
		}
	}
	return tile{}, false
}

// Session window of a member on the last paint
func (a *arena) tileOf(name string) (tile, bool) {
	for _, tl := range a.tiles {
		if tl.name == name {
			return tl, true
		}
	}
	return tile{}, false
}
