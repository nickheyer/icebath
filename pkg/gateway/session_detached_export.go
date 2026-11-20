package gateway

import "context"

// SessionEnvSource is the source a turn's session variables name, the local view for a conversation its chat moved past
func SessionEnvSource(ctx context.Context, runner any, source *SessionSource) (*SessionSource, error) {
	key := detachedRouteOf(source)
	if key == nil {
		return source, nil
	}
	sessionID, _ := detachedSessionID(*key)
	authority, err := activeAuthority(ctx, runner)
	if err != nil {
		return nil, err
	}
	return localSource(ctx, authority, sessionID, strVal(source.UserID))
}
