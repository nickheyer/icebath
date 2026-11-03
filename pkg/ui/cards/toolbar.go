package cards

import (
	"slices"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Button of a toolbar, its action, its words and whether it stands pressed
type Button struct {
	ID    string
	Label string
	On    bool
	// Buttons of a higher rank leave the bar first when it runs out of room
	Rank int
	// Gap drawn before the button
	Gap bool
}

// Row of buttons across a painted card, each labeled with its key while they fit, the least needed dropped first
type Toolbar struct {
	Keys *keys.Map
	// Action under the pointer, drawn raised
	Hover string
}

// Draws the buttons that fit across a row, reporting the box of each through the callback
func (tb Toolbar) Paint(p *Painter, x, y, w int, btns []Button, hit func(id string, x, y, w, h int)) {
	t := p.Theme()
	p.Fill(x, y, w, 1, theme.Mix(t.Surface, t.Bg, 0.5))
	btns = slices.Clone(btns)
	keyed := true
	width := func(keyed bool) int {
		n := 1
		for _, b := range btns {
			if b.Gap {
				n++
			}
			n += tb.width(b, keyed) + 1
		}
		return n
	}
	if width(true) > w {
		keyed = false
	}
	for rank := 3; rank > 0 && width(keyed) > w; rank-- {
		btns = slices.DeleteFunc(btns, func(b Button) bool { return b.Rank >= rank })
	}
	cx := x + 1
	for _, b := range btns {
		if b.Gap {
			cx++
		}
		bw := tb.button(p, cx, y, b, keyed)
		if hit != nil {
			hit(b.ID, cx, y, bw, 1)
		}
		cx += bw + 1
	}
}

// Key shown beside a button, empty while the map binds none
func (tb Toolbar) key(id string) string {
	if tb.Keys == nil {
		return ""
	}
	return tb.Keys.Display(id)
}

// Cells a button takes, its key included when keyed
func (tb Toolbar) width(b Button, keyed bool) int {
	w := ansi.StringWidth(b.Label) + 2
	if key := tb.key(b.ID); keyed && key != "" {
		w += ansi.StringWidth(key) + 1
	}
	return w
}

// Draws one button, lit when pressed, raised under the pointer, its key dim after the label
func (tb Toolbar) button(p *Painter, x, y int, b Button, keyed bool) int {
	t := p.Theme()
	bg, fg, keyFg := theme.Mix(t.Surface, t.Steel, 0.7), t.Text, t.TextDim
	switch {
	case b.On:
		bg, fg, keyFg = t.Ice, t.Bg, theme.Mix(t.Bg, t.Ice, 0.35)
	case tb.Hover == b.ID:
		bg = t.SteelBright
	}
	w := p.Label(x, y, " "+b.Label, fg, bg, b.On)
	if key := tb.key(b.ID); keyed && key != "" {
		w += p.Label(x+w, y, " "+key, keyFg, bg, false)
	}
	return w + p.Label(x+w, y, " ", fg, bg, false)
}
