package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Test override of the sessions index, the live profile index when empty
var mirrorSessionsIndex string

// Active profile sessions.json at call time unless a test pinned one
func resolveSessionsIndex(ctx context.Context) string {
	if mirrorSessionsIndex != "" {
		return mirrorSessionsIndex
	}
	return filepath.Join(home.Dir(ctx), "sessions", "sessions.json")
}

// Origin user id of an index entry as text
func originUserID(entry *pyjson.Object) string {
	origin, _ := entry.Vals["origin"].(*pyjson.Object)
	if origin == nil || !pyrepr.Truthy(origin.Vals["user_id"]) {
		return ""
	}
	return pyrepr.Str(origin.Vals["user_id"])
}

// MirrorOptions are the optional thread, sender, role and exact session of a mirror
type MirrorOptions struct {
	SourceLabel string
	ThreadID    *string
	UserID      *string
	Role        string
	SessionID   string
}

// Optional text the way Python formats None
func optText(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

// Appends a delivery mirror message to the target session transcript, false when no session matches or the write fails
func MirrorToSession(ctx context.Context, platform, chatID, messageText string, opts MirrorOptions) bool {
	if opts.SourceLabel == "" {
		opts.SourceLabel = "cli"
	}
	if opts.Role == "" {
		opts.Role = "assistant"
	}
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = findMirrorSessionID(ctx, platform, chatID, opts.ThreadID, opts.UserID)
	}
	if sessionID == "" {
		slog.Warn(fmt.Sprintf("Mirror: no session found for %s:%s thread=%s user=%s (explicit_id=none, origin-scan bailed)",
			platform, chatID, optText(opts.ThreadID), optText(opts.UserID)))
		return false
	}
	if err := appendMirrorMessage(ctx, sessionID, opts.Role, messageText); err != nil {
		slog.Warn(fmt.Sprintf("Mirror failed for %s:%s thread=%s user=%s session=%s: %v", platform, chatID,
			optText(opts.ThreadID), optText(opts.UserID), sessionID, err))
		return false
	}
	slog.Debug(fmt.Sprintf("Mirror: wrote to session %s (from %s)", sessionID, opts.SourceLabel))
	return true
}

// Active session of a platform chat from state.db, then the pre migration sessions.json scan
func findMirrorSessionID(ctx context.Context, platform, chatID string, threadID, userID *string) string {
	if id, err := func() (string, error) {
		db, err := state.Acquire(ctx, filepath.Join(home.Dir(ctx), "state.db"))
		if err != nil {
			return "", err
		}
		defer state.ReleaseOrClose(db)
		return state.FindSessionByOrigin(ctx, db, platform, chatID, threadID, userID)
	}(); err != nil {
		slog.Debug(fmt.Sprintf("Mirror state.db session lookup failed: %v", err))
	} else if id != "" {
		return id
	}
	raw, err := os.ReadFile(resolveSessionsIndex(ctx))
	if err != nil {
		return ""
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	data, isObj := parsed.(*pyjson.Object)
	if err != nil || !isObj {
		return ""
	}
	matches := func(entry *pyjson.Object) bool {
		origin, _ := entry.Vals["origin"].(*pyjson.Object)
		if origin == nil {
			origin = pyjson.NewObject()
		}
		plat := origin.Vals["platform"]
		if !pyrepr.Truthy(plat) {
			plat = entry.Vals["platform"]
			if _, ok := entry.Get("platform"); !ok {
				plat = ""
			}
		}
		chat, ok := origin.Get("chat_id")
		if !ok {
			chat = ""
		}
		thread := origin.Vals["thread_id"]
		if !pyrepr.Truthy(thread) {
			thread = ""
		}
		platText, isStr := plat.(string)
		return isStr && pystr.Lower(platText) == pystr.Lower(platform) && pyrepr.Str(chat) == chatID &&
			(threadID == nil || pyrepr.Str(thread) == *threadID)
	}
	var candidates []*pyjson.Object
	for _, k := range data.Keys {
		if entry, ok := data.Vals[k].(*pyjson.Object); ok && !strings.HasPrefix(k, "_") && matches(entry) {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if userID != nil && *userID != "" {
		var exact []*pyjson.Object
		for _, e := range candidates {
			if originUserID(e) == *userID {
				exact = append(exact, e)
			}
		}
		if len(exact) > 0 {
			candidates = exact
		} else if len(candidates) > 1 {
			return ""
		}
	} else if len(candidates) > 1 {
		users := map[string]bool{}
		for _, e := range candidates {
			if u := pystr.Strip(originUserID(e)); u != "" {
				users[u] = true
			}
		}
		if len(users) > 1 {
			return ""
		}
	}
	best := candidates[0]
	for _, e := range candidates[1:] {
		if pyrepr.Str(updatedAt(e)) > pyrepr.Str(updatedAt(best)) {
			best = e
		}
	}
	id, _ := best.Vals["session_id"].(string)
	return id
}

// updated_at of an index entry, empty when missing
func updatedAt(e *pyjson.Object) any {
	if v, ok := e.Get("updated_at"); ok {
		return v
	}
	return ""
}

// Appends one role and content row to the session, returning the write failure
func appendMirrorMessage(ctx context.Context, sessionID, role, content string) error {
	db, err := state.Acquire(ctx, filepath.Join(home.Dir(ctx), "state.db"))
	if err != nil {
		return err
	}
	defer state.ReleaseOrClose(db)
	_, err = db.AppendMessage(sessionID, &state.Message{Role: role, Content: content})
	return err
}
