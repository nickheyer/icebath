package tui

// Commands the terminal UI runs natively in place of the classic REPL handlers
func (a *app) nativeCommands() []slashCommand {
	var out []slashCommand
	for _, set := range [][]slashCommand{a.toolsCommands(), a.snapshotCommands(), a.profileArchiveCommands(), a.journeyCommands(), a.worktreeCommands(), a.handoffCommands(),
		a.billingCommands(), a.subscriptionCommands()} {
		out = append(out, set...)
	}
	return out
}
