package tuiserver

// Latest activity of the sessions closed under a resolved profile home, zero when none closed there
func ClosedSessionActivity(resolvedHome string) float64 {
	closedSessionActivity.Lock()
	defer closedSessionActivity.Unlock()
	return closedSessionActivity.byHome[resolvedHome]
}
