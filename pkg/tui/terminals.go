package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Opens a terminal window on a pty in the session's folder, the user's shell or one command line, as a tab of the focused window when asked
func (a *app) openTerminal(line string, asTab bool) tea.Cmd {
	a.terminalCount++
	env := a.cardEnv()
	env.Root = a.store.sessionCwd()
	env.Version = infoText(infoOf(a.store.ui.Info).Version)
	var t *cards.Terminal
	if line = strings.TrimSpace(line); line != "" {
		t = cards.NewTerminalCommand(env, a.terminalCount, a.store.sessionCwd(), line)
	} else {
		t = cards.NewTerminal(env, a.terminalCount, a.store.sessionCwd())
	}
	open := a.terminals[:0]
	for _, kept := range a.terminals {
		if card, _ := a.wm.Find(kept.Key()); card == kept {
			open = append(open, kept)
		}
	}
	a.terminals = append(open, t)
	if asTab {
		return a.wm.OpenTab(t)
	}
	return a.wm.OpenHere(t)
}

// Ends the program of every terminal window still open, for icebath quitting
func (a *app) closeTerminals() {
	for _, t := range a.terminals {
		t.Close()
	}
	a.terminals = nil
}

// Commands that open a terminal window or a terminal tab
func (a *app) terminalCommands() []slashCommand {
	return []slashCommand{
		{name: "terminal", help: tr("slashCmd.core.help.terminal"), usage: "[command]", run: func(r *slashRun) tea.Cmd { return a.openTerminal(r.arg, false) }},
		{name: "terminal-tab", help: tr("slashCmd.core.help.terminalTab"), usage: "[command]", run: func(r *slashRun) tea.Cmd { return a.openTerminal(r.arg, true) }},
	}
}
