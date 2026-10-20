package main

import (
	"fmt"
	"image/color"
	"math/rand/v2"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type rosterTick time.Time

type daemon struct {
	name  string
	state int
}

// Agent roster whose states drift and whose last row is a spawn button
type roster struct {
	daemons []daemon
	rng     *rand.Rand
	pal     theme.Theme
	h       int
}

var states = []string{"idle", "scan", "run", "halt"}

func newRoster() *roster {
	return &roster{
		daemons: []daemon{{"sentinel", 2}, {"wraith", 1}, {"courier", 0}, {"oracle", 2}, {"lockpick", 3}},
		rng:     rand.New(rand.NewPCG(3, 5)),
		pal:     theme.Blackwall(),
	}
}

func (r *roster) Title() string { return "DAEMONS" }
func (r *roster) Glyph() string { return "☍" }
func (r *roster) Init() tea.Cmd { return r.schedule() }

func (r *roster) schedule() tea.Cmd {
	return tea.Tick(900*time.Millisecond, func(at time.Time) tea.Msg { return rosterTick(at) })
}

func (r *roster) Update(msg tea.Msg) (deck.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case rosterTick:
		r.daemons[r.rng.IntN(len(r.daemons))].state = r.rng.IntN(len(states))
		return r, r.schedule()
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft && msg.Y == r.h-1 {
			return r, func() tea.Msg { return spawnMsg{} }
		}
	}
	return r, nil
}

func (r *roster) View(w, h int) string {
	r.h = h
	p := r.pal
	cols := []color.Color{p.TextDim, p.Ice, p.Alive, p.Alarm}
	lines := make([]string, 0, h)
	for _, d := range r.daemons {
		if len(lines) >= h-1 {
			break
		}
		tone := lipgloss.NewStyle().Foreground(cols[d.state])
		lines = append(lines, tone.Render("●")+" "+lipgloss.NewStyle().Foreground(p.Text).Render(fmt.Sprintf("%-10s", d.name))+" "+tone.Render(states[d.state]))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(p.Bg).Background(p.Accent).Bold(true).Render(" + spawn probe "))
	return strings.Join(lines, "\n")
}

func (r *roster) Status() string {
	n := 0
	for _, d := range r.daemons {
		if d.state == 2 {
			n++
		}
	}
	return fmt.Sprintf("%d run", n)
}
