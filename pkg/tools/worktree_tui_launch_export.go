package tools

import (
	"context"
	"time"
)

// Whether a resumable session or pending gateway work still has its cwd inside a retained tree
func RetainedTreeIsLive(dbPath, worktreePath string, pendingOnly bool) bool {
	return retainedTreeIsLive(dbPath, worktreePath, pendingOnly)
}

// Why the gateway still needs a retained tree, empty when nothing serves it
func TreeStillServed(ctx context.Context, dbPath, worktreePath string) string {
	return treeStillServed(ctx, dbPath, worktreePath)
}

// Relocks a tree an exiting launcher retains under the session that uses it, the name hermes tests replace
var RetainWorktreeForSession = retainWorktreeForSession

// Whether another live process still works in the tree after the grace period
func WorktreeStillInUse(path string, grace time.Duration) bool {
	return worktreeStillInUse(path, grace)
}
