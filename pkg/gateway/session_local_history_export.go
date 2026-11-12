package gateway

import "context"

// Local session an exact id or title names, projected to its compression tip
func ResolveTitledSession(ctx context.Context, a *SessionAuthority, actor Principal, name any) (*SessionRef, error) {
	return resolveTitledSession(ctx, a, actor, name, false)
}

// Reopens one stored local session under the authority as its owned live session
func ReopenLocalSession(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	return reopenLocalSession(ctx, a, ref)
}

// Binds a native owner connection of the authority profile as a legacy transport that may create sessions
func BindNativeTransport(a *SessionAuthority, actor Principal, identity map[string]any) {
	bindNativeTransport(a, actor, identity)
}

// Restores every boot receipt of the authority, scheduling pending input when asked
func RecoverLocalSessions(ctx context.Context, a *SessionAuthority, schedule bool) error {
	return recoverLocalSessions(ctx, a, schedule)
}
