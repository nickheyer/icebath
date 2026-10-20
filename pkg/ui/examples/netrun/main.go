// Demo deck with a streaming trace, an ICE visualizer, an uplink prompt, and a daemon roster
package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/deck"
)

// Emitted by the roster when its spawn button is clicked
type spawnMsg struct{}

type app struct {
	deck   *deck.Model
	probes int
}

func main() {
	a := app{deck: deck.New(
		deck.WithCards(newTrace(), newIce(), newUplink(), newRoster()),
		deck.WithWindowTitle("icebath"),
	)}
	if _, err := tea.NewProgram(a).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (a app) Init() tea.Cmd { return a.deck.Init() }

func (a app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spawnMsg:
		return a.spawn()
	case tea.KeyPressMsg:
		if !a.deck.Capturing() && (msg.String() == "n" || msg.String() == "+") {
			return a.spawn()
		}
	}
	_, cmd := a.deck.Update(msg)
	return a, cmd
}

func (a app) spawn() (tea.Model, tea.Cmd) {
	a.probes++
	return a, a.deck.Add(newProbe(a.probes))
}

func (a app) View() tea.View { return a.deck.View() }
