package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
)

// Hands the gateway session store the tree recorded for the worktree flag and the folder this command line's sessions work in
func init() {
	gatewaycli.RecordedWorktree = func() map[string]string {
		activeWorktreeMu.Lock()
		defer activeWorktreeMu.Unlock()
		return activeWorktree
	}
	gatewaycli.LaunchFolder = func() string { return sessionFolder(context.Background()) }
}
