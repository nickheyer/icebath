package main

import (
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type iceTick time.Time

// Spectrum of interference bars that never stops moving
type ice struct {
	t   float64
	pal theme.Theme
}

var blocks = []rune("▁▂▃▄▅▆▇█")

func newIce() *ice { return &ice{pal: theme.Blackwall()} }

func (c *ice) Title() string { return "ICE" }
func (c *ice) Glyph() string { return "◈" }
func (c *ice) Init() tea.Cmd { return c.schedule() }

func (c *ice) schedule() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(at time.Time) tea.Msg { return iceTick(at) })
}

func (c *ice) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	if _, ok := msg.(iceTick); ok {
		c.t += 0.05
		return c, c.schedule()
	}
	return c, nil
}

func (c *ice) View(w, h int) string {
	if w < 1 || h < 1 {
		return ""
	}
	levels := make([]int, w)
	for x := range levels {
		fx := float64(x)
		v := 0.5 + 0.5*math.Sin(fx*0.35+c.t*2.1)*math.Sin(fx*0.11-c.t*0.7)
		v = 0.15 + 0.85*v*v
		levels[x] = int(v * float64(h) * 8)
	}
	shades := lipgloss.Blend1D(max(h, 2), c.pal.Pulse, c.pal.Ice)
	var sb strings.Builder
	for row := range h {
		fromBottom := h - 1 - row
		line := make([]rune, w)
		for x, lv := range levels {
			full, rem := lv/8, lv%8
			switch {
			case fromBottom < full:
				line[x] = '█'
			case fromBottom == full && rem > 0:
				line[x] = blocks[rem-1]
			default:
				line[x] = ' '
			}
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(shades[row]).Render(string(line)))
		if row < h-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func (c *ice) Status() string { return "live" }
