package tuiserver

import "context"

// Closes one live session through the teardown funnel with the given end reason, false when none was live
func CloseSessionByID(ctx context.Context, sid, endReason string) bool {
	return closeSessionByID(ctx, sid, endReason, nil)
}

// Latest activity of the sessions closed under a resolved profile home, zero when none closed there
func ClosedSessionActivity(resolvedHome string) float64 {
	closedSessionActivity.Lock()
	defer closedSessionActivity.Unlock()
	return closedSessionActivity.byHome[resolvedHome]
}
