package gateway

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// Refusal of a session a window or profile has no room to open, its message naming the limit, the session and one to close
func sessionLimitRefusal(ctx context.Context, c *AuthorityConnection, sessionID string) error {
	a := c.authority
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	c.mu.Lock()
	if _, held := c.subscriptions[sessionID]; sessionID != "" && held {
		c.mu.Unlock()
		return nil
	}
	window := make([]string, 0, len(c.subscriptions))
	for id := range c.subscriptions {
		window = append(window, id)
	}
	c.mu.Unlock()
	if limit := settings.SessionsMaxPerWindow.Get(scoped); len(window) >= limit {
		return limitRefusal(ctx, a, sessionID, window, fmt.Sprintf("This window already has %d sessions open", len(window)),
			settings.SessionsMaxPerWindow.Key)
	}
	a.mu.Lock()
	open := []string{}
	for id, live := range a.Sessions {
		if id != sessionID && len(live.Subscribers) > 0 {
			open = append(open, id)
		}
	}
	a.mu.Unlock()
	if limit := settings.SessionsMaxOpen.Get(scoped); len(open) >= limit {
		return limitRefusal(ctx, a, sessionID, open, fmt.Sprintf("This profile already has %d sessions open", len(open)),
			settings.SessionsMaxOpen.Key)
	}
	return nil
}

// Refusal naming the session left closed, the oldest idle one among the open ones and the setting that raises the limit
func limitRefusal(ctx context.Context, a *SessionAuthority, sessionID string, open []string, lead, key string) error {
	subject := "the new session"
	if title := sessionTitle(ctx, a, sessionID); title != "" {
		subject = "“" + title + "”"
	}
	message := lead + ", so " + subject + " was not opened. Close a session first"
	if oldest := oldestIdleTitle(ctx, a, open); oldest != "" {
		message += " (the oldest idle one is “" + oldest + "”)"
	}
	message += ", or raise " + key + "."
	field := "session_id"
	refusal := state.NewRuntimeStoreError("session_limit", &field, &message)
	return &codedRefusal{RuntimeStoreError: refusal, code: 4019, message: message}
}

// Stored title of a session, empty when it has none
func sessionTitle(ctx context.Context, a *SessionAuthority, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	row, err := a.DB.GetSession(ctx, sessionID)
	if err != nil || row == nil {
		return ""
	}
	title, _ := row.ValueOr("title", nil).(string)
	return strings.TrimSpace(title)
}

// Title of the open session that has been idle the longest, its id when it has no title, empty when every one runs a turn
func oldestIdleTitle(ctx context.Context, a *SessionAuthority, open []string) string {
	best, bestAt := "", 0.0
	for _, id := range open {
		handle, err := a.handle(ctx, SessionRef{ProfileID: a.ProfileID, SessionID: id})
		if err != nil || handle.ExecutionState == "running" {
			continue
		}
		row, err := a.DB.GetSession(ctx, id)
		if err != nil || row == nil {
			continue
		}
		at := rowFloat(row, "last_activity_at")
		if at == 0 {
			at = rowFloat(row, "started_at")
		}
		if best == "" || at < bestAt {
			best, bestAt = id, at
			if title, _ := row.ValueOr("title", nil).(string); strings.TrimSpace(title) != "" {
				best = strings.TrimSpace(title)
			}
		}
	}
	return best
}

// Number a row column holds, zero for NULL or anything else
func rowFloat(row interface{ ValueOr(string, any) any }, key string) float64 {
	switch n := row.ValueOr(key, nil).(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}
