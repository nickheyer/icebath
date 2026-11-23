package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Stored title of a live session, else its pending title
func sessionLiveTitle(ctx context.Context, session *tuihost.Session, key string) (title string) {
	title = strings.TrimSpace(strOr(session.Get("pending_title")))
	pending := title
	defer func() {
		if recover() != nil {
			title = pending
		}
	}()
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return title
	}
	stored, _, err := db.Title(key)
	if err != nil {
		return pending
	}
	if stored != "" {
		return strings.TrimSpace(stored)
	}
	return strings.TrimSpace(pending)
}

// Method of the server request a session is blocked on, empty when none
func sessionPendingKind(sid string) string { return tuihost.PendingKind(sid) }

// Stored session id of a live record, its agent's id first, then its key, then the fallback
func sessionLookupKey(session *tuihost.Session, fallback string) string {
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		return a.SessionID()
	}
	if key := strOr(session.Get("session_key")); key != "" {
		return key
	}
	return fallback
}

// Number of a slot as a float, the fallback when the slot is falsy
func floatOr(v any, fallback float64) float64 {
	if !truthy(v) {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return fallback
}
