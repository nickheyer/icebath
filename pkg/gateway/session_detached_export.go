package gateway

import "context"

// SessionEnvSource is the source a turn's session variables name, the local view for a conversation its chat moved past
func SessionEnvSource(ctx context.Context, runner any, source *SessionSource) (*SessionSource, error) {
	key := detachedRouteOf(source)
	if key == nil {
		return source, nil
	}
	sessionID, _ := detachedSessionID(*key)
	return localSource(ctx, activeAuthority(runner), sessionID, strVal(source.UserID))
}
