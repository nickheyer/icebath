package app

import "github.com/nickheyer/icebath/pkg/gatewaycli"

// Hands the gateway session store the tree recorded for the worktree flag
func init() {
	gatewaycli.RecordedWorktree = func() map[string]string {
		activeWorktreeMu.Lock()
		defer activeWorktreeMu.Unlock()
		return activeWorktree
	}
}
