package cards

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Section of a dock, its lines, the clickable rows among them and how it takes height
type Section struct {
	ID    string
	Title string
	// Count shown at the right of the title while nothing is scrolled
	Count string
	Lines []string
	Rows  []Row
	// Whether the section shares the height left after the fixed ones instead of taking its own
	Flex bool
	// Whether the section follows its end as lines arrive, until scrolled up
	Tail bool
}

// Clickable row of a section, the lines it spans and what it stands for
type Row struct {
	ID       string
	First, N int
}

// Box of cells a section or row took on the last paint
type Box struct{ X, Y, W, H int }

// Reports whether a cell lies in the box
func (b Box) Contains(x, y int) bool { return x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H }

// Stack of sections that fold, scroll and share the height of a dock, the flex ones split by a draggable rule
type Sections struct {
	// Whether each section is open, every section open until folded
	Open map[string]bool
	// Share of the flex height the first flex section takes
	Share float64
	// Action under the pointer, its title drawn raised
	Hover  string
	scroll map[string]int
	// Whether each tailing section has been scrolled up off its end
	held  map[string]bool
	rects map[string]Box
	split int
}

// Sections with the first flex one taking the share of the flex height
func NewSections(share float64) Sections {
	return Sections{Open: map[string]bool{}, Share: share, scroll: map[string]int{}, held: map[string]bool{}, rects: map[string]Box{}}
}

// Reports whether the section is open, which it is until folded
func (d *Sections) IsOpen(id string) bool {
	open, ok := d.Open[id]
	return !ok || open
}

// Folds the section or opens it again at its top, a tailing one back on its end
func (d *Sections) Toggle(id string) {
	d.Open[id] = !d.IsOpen(id)
	if d.Open[id] {
		d.scroll[id] = 0
		d.held[id] = false
	}
}

// Opens the section
func (d *Sections) Show(id string) { d.Open[id] = true }

// Scrolls the section back to its top
func (d *Sections) Top(id string) { d.scroll[id] = 0 }

// Reports whether a tailing section still follows its end
func (d *Sections) Follows(id string) bool { return !d.held[id] }

// Scrolls a section by a count of rows, a tailing one leaving its end once scrolled up and following it again once scrolled back down
func (d *Sections) Scroll(id string, rows int, tail bool) {
	d.scroll[id] = max(d.scroll[id]+rows, 0)
	if !tail {
		return
	}
	if rows < 0 {
		d.held[id] = true
	}
	if d.scroll[id] == 0 && rows > 0 {
		d.held[id] = false
	}
}

// Box a section took on the last paint, its title row included, false when it was not painted
func (d *Sections) Rect(id string) (Box, bool) {
	b, ok := d.rects[id]
	return b, ok
}

// Id of the section holding a cell of the last paint, false between them
func (d *Sections) At(x, y int) (string, bool) {
	for id, b := range d.rects {
		if b.Contains(x, y) {
			return id, true
		}
	}
	return "", false
}

// Row of the rule between the first two flex sections on the last paint, negative while there is none
func (d *Sections) Split() int { return d.split }

// First line of a section shown, a tailing one at its end unless held, the rest clamped to their scroll
func (d *Sections) top(s Section, h int) int {
	n := len(s.Lines)
	if h <= 0 || n <= h {
		d.scroll[s.ID] = 0
		return 0
	}
	if s.Tail && !d.held[s.ID] {
		d.scroll[s.ID] = 0
		return n - h
	}
	d.scroll[s.ID] = min(d.scroll[s.ID], n-h)
	if s.Tail {
		return n - h - d.scroll[s.ID]
	}
	return d.scroll[s.ID]
}

// Rows each open section gets, the fixed ones their size in order, the flex ones the rest split by the share and then evenly
func (d *Sections) allocate(total int, secs []Section) map[string]int {
	out := map[string]int{}
	avail := max(total-len(secs), 0)
	want := func(s Section) int {
		if !d.IsOpen(s.ID) {
			return 0
		}
		return len(s.Lines)
	}
	var flex []Section
	for _, s := range secs {
		if s.Flex {
			flex = append(flex, s)
			continue
		}
		n := min(want(s), avail)
		out[s.ID], avail = n, avail-n
	}
	wanted := 0
	for _, s := range flex {
		wanted += want(s)
	}
	switch {
	case len(flex) == 0:
	case wanted <= avail:
		for _, s := range flex {
			out[s.ID] = want(s)
		}
	case len(flex) == 1:
		out[flex[0].ID] = avail
	default:
		first := flex[0]
		rest := flex[1:]
		restWant := wanted - want(first)
		switch {
		case want(first) == 0:
			out[first.ID] = 0
		case restWant == 0:
			out[first.ID] = avail
		default:
			out[first.ID] = min(want(first), max(int(float64(avail)*d.Share), 3))
		}
		left := avail - out[first.ID]
		if left > restWant {
			left = restWant
			out[first.ID] = avail - left
		}
		for i, s := range rest {
			n := left / (len(rest) - i)
			out[s.ID], left = min(n, want(s)), left-min(n, want(s))
		}
	}
	return out
}

// Draws the sections down a box, each under a title row that folds it, reporting the box of every title and row through the callback and tinting rows by the color the callback gives
func (d *Sections) Paint(p *Painter, x, y, w, h int, secs []Section, hit func(id string, x, y, w, h int), rowBg func(id string) color.Color) {
	t := p.Theme()
	d.split = -1
	for id := range d.rects {
		delete(d.rects, id)
	}
	if w < 4 || h < 2 {
		return
	}
	heights := d.allocate(h, secs)
	cy := y
	flexSeen := 0
	for _, s := range secs {
		if cy >= y+h {
			break
		}
		sh := heights[s.ID]
		top := d.top(s, sh)
		shown := ""
		if sh < len(s.Lines) && sh > 0 {
			shown = fmt.Sprintf("%d-%d of %d", top+1, top+sh, len(s.Lines))
		}
		p.Styled(x, cy, w, 1, d.head(t, s, shown, w))
		if hit != nil {
			hit("sec:"+s.ID, x, cy, w, 1)
		}
		d.rects[s.ID] = Box{x, cy, w, 1 + sh}
		cy++
		if sh <= 0 {
			continue
		}
		p.Lines(x, cy, w, sh, s.Lines[top:min(top+sh, len(s.Lines))])
		for _, r := range s.Rows {
			lo, hi := max(r.First-top, 0), min(r.First+r.N-top, sh)
			if lo >= hi {
				continue
			}
			if hit != nil {
				hit(r.ID, x, cy+lo, w, hi-lo)
			}
			if rowBg != nil {
				if bg := rowBg(r.ID); bg != nil {
					p.TintBg(x, cy+lo, w, hi-lo, bg)
				}
			}
		}
		if s.Flex {
			flexSeen++
			if flexSeen == 1 && len(secs) > 0 {
				d.split = cy + sh
			}
		}
		cy += sh
	}
	if flexSeen < 2 {
		d.split = -1
	}
}

// Title row of a section, a fold mark, its name, a rule, what is shown of it and its count
func (d *Sections) head(t theme.Theme, s Section, shown string, w int) string {
	mark := t.Glyphs.Down
	if !d.IsOpen(s.ID) {
		mark = t.Glyphs.Caret
	}
	hot := d.Hover == "sec:"+s.ID
	title := lipgloss.NewStyle().Foreground(t.Ice).Bold(true).Render(strings.ToUpper(s.Title))
	if hot {
		title = lipgloss.NewStyle().Foreground(t.Text).Background(t.Steel).Bold(true).Render(" " + strings.ToUpper(s.Title) + " ")
	}
	left := lipgloss.NewStyle().Foreground(t.TextDim).Render(mark) + " " + title
	right := render.Dim(s.Count, t)
	if shown != "" {
		right = render.Dim(shown, t)
	}
	gap := w - ansi.StringWidth(left) - ansi.StringWidth(right) - 2
	if gap < 1 {
		return left
	}
	return left + " " + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Rule, gap)) + " " + right
}
