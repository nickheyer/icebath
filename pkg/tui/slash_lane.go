package tui

// Commands the terminal UI runs natively
func (a *app) nativeCommands() []slashCommand {
	var out []slashCommand
	for _, set := range [][]slashCommand{a.toolsCommands(), a.snapshotCommands(), a.profileArchiveCommands(), a.journeyCommands(), a.worktreeCommands(), a.handoffCommands()} {
		out = append(out, set...)
	}
	return out
}
