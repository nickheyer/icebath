package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type probeTick struct {
	n int
}

// Breach attempt that fills a bar and then reports done
type probe struct {
	n   int
	p   float64
	rng *rand.Rand
	pal theme.Theme
}

func newProbe(n int) *probe {
	return &probe{n: n, rng: rand.New(rand.NewPCG(uint64(n), 9)), pal: theme.Blackwall()}
}

func (p *probe) Title() string { return fmt.Sprintf("PROBE-%02d", p.n) }
func (p *probe) Glyph() string { return "◎" }
func (p *probe) Init() tea.Cmd { return p.schedule() }

func (p *probe) schedule() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return probeTick{n: p.n} })
}

func (p *probe) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	if tick, ok := msg.(probeTick); ok && tick.n == p.n && p.p < 1 {
		p.p = math.Min(1, p.p+0.004+p.rng.Float64()*0.02)
		return p, p.schedule()
	}
	return p, nil
}

func (p *probe) View(w, h int) string {
	bar := max(1, w-6)
	filled := int(float64(bar)*p.p + 0.5)
	shades := lipgloss.Blend1D(max(bar, 2), p.pal.Pulse, p.pal.Ice)
	var sb strings.Builder
	for i := range bar {
		if i < filled {
			sb.WriteString(lipgloss.NewStyle().Foreground(shades[i]).Render("█"))
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(p.pal.Steel).Render("░"))
		}
	}
	label := fmt.Sprintf("%3d%%", int(p.p*100))
	if p.p >= 1 {
		label = "done"
	}
	line := sb.String() + " " + lipgloss.NewStyle().Foreground(p.pal.Accent).Render(label)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, line)
}

func (p *probe) Status() string {
	if p.p >= 1 {
		return "breached"
	}
	return fmt.Sprintf("%d%%", int(p.p*100))
}
