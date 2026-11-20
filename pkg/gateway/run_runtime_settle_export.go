package gateway

import "context"

// Ends every ACP session of the authority nobody is viewing, the stop being their end
func EndIdleACPSessions(ctx context.Context, a *SessionAuthority) (int, error) {
	return endIdleACPSessions(ctx, a)
}
