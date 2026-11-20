package gateway

import "context"

// Restores every boot receipt of the authority, scheduling pending input when asked
func RecoverLocalSessions(ctx context.Context, a *SessionAuthority, schedule bool) error {
	return recoverLocalSessions(ctx, a, schedule)
}
