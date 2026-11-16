package tui

import (
	tea "charm.land/bubbletea/v2"
)

// Slash commands that drive the window manager itself
func (a *app) wmCommands() []slashCommand {
	return []slashCommand{
		{name: "palette", help: "open the command palette", run: func(r *slashRun) tea.Cmd {
			return tea.Batch(r.ran(), a.wm.OpenPalette())
		}},
	}
}
