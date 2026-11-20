package state

import "context"

// Backfills the api_content sidecar onto the newest active user row when its content still matches
func (d *SessionDB) SetLatestUserAPIContent(ctx context.Context, sessionID string, content any, apiContent string) (int64, error) {
	return d.writeRowcount(ctx, "UPDATE messages SET api_content = ? WHERE id = (SELECT id FROM messages "+
		"WHERE session_id = ? AND role = 'user' AND active = 1 ORDER BY id DESC LIMIT 1"+
		") AND content IS ?", []any{scrubSurrogates(apiContent), sessionID, encodeMessageContent(content)}, nil)
}

// Backfills the api_content sidecar onto one known active user row whose content still matches
func (d *SessionDB) SetMessageAPIContent(ctx context.Context, sessionID string, rowID int64, content any, apiContent string) (int64, error) {
	if sessionID == "" || rowID <= 0 {
		return 0, nil
	}
	return d.writeRowcount(ctx, "UPDATE messages SET api_content = ? WHERE id = ? AND session_id = ? "+
		"AND role = 'user' AND active = 1 AND content IS ?", []any{scrubSurrogates(apiContent), rowID, sessionID, encodeMessageContent(content)}, nil)
}
