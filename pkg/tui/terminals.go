package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Opens a terminal window on a pty in the session's folder, the user's shell or one command line, as a tab of the focused window when asked
func (a *app) openTerminal(line string, asTab bool) tea.Cmd {
	a.terminalCount++
	env := a.terminalEnv()
	var t *cards.Terminal
	if line = strings.TrimSpace(line); line != "" {
		t = cards.NewTerminalCommand(env, a.terminalCount, a.store.sessionCwd(), line)
	} else {
		t = cards.NewTerminal(env, a.terminalCount, a.store.sessionCwd())
	}
	a.keepTerminal(t)
	if asTab {
		return a.wm.OpenTab(t)
	}
	return a.wm.OpenHere(t)
}

// Runs a program a setup flow hands over, a sign-in CLI or an installer, in a terminal window of its own beside the session, handing back
// how it ended and what it printed so a token it shows is picked up without copying it anywhere
func (a *app) runInTerminal(argv []string, done func(cards.TerminalEnd) tea.Cmd) tea.Cmd {
	a.terminalCount++
	t := cards.NewTerminalArgv(a.terminalEnv(), a.terminalCount, a.store.sessionCwd(), argv)
	t.OnEnd = done
	a.keepTerminal(t)
	return a.wm.OpenHere(t)
}

// Card environment of a terminal window, rooted at the session's folder
func (a *app) terminalEnv() *cards.Env {
	env := a.cardEnv()
	env.Root = a.store.sessionCwd()
	env.Version = infoText(infoOf(a.store.ui.Info).Version)
	return env
}

// Tracks a terminal window for shutdown, dropping the ones no longer open
func (a *app) keepTerminal(t *cards.Terminal) {
	open := a.terminals[:0]
	for _, kept := range a.terminals {
		if card, _ := a.wm.Find(kept.Key()); card == kept {
			open = append(open, kept)
		}
	}
	a.terminals = append(open, t)
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
		{name: "terminal", help: tr(txt.SlashCmdCoreHelpTerminal), helpFn: helpOf(txt.SlashCmdCoreHelpTerminal), usage: "[command]", run: func(r *slashRun) tea.Cmd { return a.openTerminal(r.arg, false) }},
		{name: "terminal-tab", help: tr(txt.SlashCmdCoreHelpTerminalTab), helpFn: helpOf(txt.SlashCmdCoreHelpTerminalTab), usage: "[command]", run: func(r *slashRun) tea.Cmd { return a.openTerminal(r.arg, true) }},
	}
}
