package agent

// A turn nobody waits on, a persistence detached fork, a cron run or a delegated child
func isUnattendedTurn(a *AIAgent) bool {
	return a.persistDisabled || a.platform == "cron" || a.delegateDepth > 0
}
