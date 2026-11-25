package gateway

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
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
func originUserID(entry *jsonutil.Object) string {
	origin, _ := entry.Value("origin").(*jsonutil.Object)
	if origin == nil || !anyutil.IsTrue(origin.Value("user_id")) {
		return ""
	}
	return anyutil.String(origin.Value("user_id"))
}

// MirrorOptions are the optional thread, sender, role and exact session of a mirror
type MirrorOptions struct {
	SourceLabel string
	ThreadID    *string
	UserID      *string
	Role        string
	SessionID   string
}

// Optional text for a log line, none when absent
func optText(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

// Transcript writer of a mirrored message, a seam for the write failure tests
var appendMirrorMessage = appendMirrorMessageSQLite

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
		found, err := findMirrorSessionID(ctx, platform, chatID, opts.ThreadID, opts.UserID)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("Mirror failed for %s:%s thread=%s user=%s session=none: %v", platform, chatID,
				optText(opts.ThreadID), optText(opts.UserID), err))
			return false
		}
		sessionID = found
	}
	if sessionID == "" {
		pkgLog().Warn(fmt.Sprintf("Mirror: no session found for %s:%s thread=%s user=%s (explicit_id=none, origin-scan bailed)",
			platform, chatID, optText(opts.ThreadID), optText(opts.UserID)))
		return false
	}
	if err := appendMirrorMessage(ctx, sessionID, opts.Role, messageText); err != nil {
		pkgLog().Warn(fmt.Sprintf("Mirror failed for %s:%s thread=%s user=%s session=%s: %v", platform, chatID,
			optText(opts.ThreadID), optText(opts.UserID), sessionID, err))
		return false
	}
	pkgLog().Debug(fmt.Sprintf("Mirror: wrote to session %s (from %s)", sessionID, opts.SourceLabel))
	return true
}

// Active session of a platform chat from state.db, then the pre migration sessions.json scan, the most recently updated match winning; the scan's shape errors returned
func findMirrorSessionID(ctx context.Context, platform, chatID string, threadID, userID *string) (string, error) {
	if id, err := func() (string, error) {
		db, err := mirrorAcquireDB(ctx, filepath.Join(home.Dir(ctx), "state.db"))
		if err != nil {
			return "", err
		}
		defer mirrorReleaseDB(db)
		return state.FindSessionByOrigin(ctx, db, platform, chatID, threadID, userID)
	}(); err != nil {
		pkgLog().Debug(fmt.Sprintf("Mirror state.db session lookup failed: %v", err))
	} else if id != "" {
		return id, nil
	}
	index := resolveSessionsIndex(ctx)
	if _, err := os.Stat(index); err != nil {
		return "", nil
	}
	raw, err := os.ReadFile(index)
	if err != nil {
		return "", nil
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return "", nil
	}
	parsed, err := jsonutil.Decode(text)
	if err != nil {
		return "", nil
	}
	data, isObj := parsed.(*jsonutil.Object)
	if !isObj {
		return "", mirrorShapeError("the sessions index", parsed, "a mapping")
	}
	var candidates []*jsonutil.Object
	for _, k := range data.Keys() {
		entry, ok := data.Value(k).(*jsonutil.Object)
		if strings.HasPrefix(k, "_") || !ok {
			continue
		}
		matched, err := mirrorEntryMatches(entry, platform, chatID, threadID)
		if err != nil {
			return "", err
		}
		if matched {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	if userID != nil && *userID != "" {
		var exact []*jsonutil.Object
		for _, e := range candidates {
			if originUserID(e) == *userID {
				exact = append(exact, e)
			}
		}
		if len(exact) > 0 {
			candidates = exact
		} else if len(candidates) > 1 {
			return "", nil
		}
	} else if len(candidates) > 1 {
		users := map[string]bool{}
		for _, e := range candidates {
			if u := strings.TrimSpace(originUserID(e)); u != "" {
				users[u] = true
			}
		}
		if len(users) > 1 {
			return "", nil
		}
	}
	best := candidates[0]
	bestAt, bestDated := updatedAt(best)
	for _, e := range candidates[1:] {
		if at, dated := updatedAt(e); dated && (!bestDated || at.After(bestAt)) {
			best, bestAt, bestDated = e, at, true
		}
	}
	return mirrorSessionIDText(best.Value("session_id"))
}

// Refusal of a decoded mirror value of the wrong kind
func mirrorShapeError(what string, v any, want string) error {
	return fmt.Errorf("%s is %s, expected %s", what, errchain.Kind(v), want)
}

// Whether an index entry's origin names the platform, chat and thread, an error for an origin that is not a mapping
func mirrorEntryMatches(entry *jsonutil.Object, platform, chatID string, threadID *string) (bool, error) {
	originValue := entry.Value("origin")
	if !anyutil.IsTrue(originValue) {
		originValue = jsonutil.NewObject()
	}
	origin, isObj := originValue.(*jsonutil.Object)
	if !isObj {
		return false, mirrorShapeError("a session origin", originValue, "a mapping")
	}
	plat := origin.Value("platform")
	if !anyutil.IsTrue(plat) {
		plat = ""
		if v, ok := entry.Get("platform"); ok {
			plat = v
		}
	}
	platText, isStr := plat.(string)
	if !isStr {
		return false, mirrorShapeError("a session platform", plat, "text")
	}
	if strings.ToLower(platText) != strings.ToLower(platform) {
		return false, nil
	}
	chat, ok := origin.Get("chat_id")
	if !ok {
		chat = ""
	}
	if anyutil.String(chat) != chatID {
		return false, nil
	}
	if threadID == nil {
		return true, nil
	}
	thread := origin.Value("thread_id")
	if !anyutil.IsTrue(thread) {
		thread = ""
	}
	return anyutil.String(thread) == *threadID, nil
}

// Instant an index entry's updated_at names, text without an offset read as local time; false when it is missing or not ISO 8601 text, which ranks it below every dated entry
func updatedAt(e *jsonutil.Object) (time.Time, bool) {
	text, ok := e.Value("updated_at").(string)
	if !ok {
		return time.Time{}, false
	}
	at, err := timeutil.ParseISO(text, time.Local)
	return at, err == nil
}

// Session id an index entry holds as bound text, empty when falsy, the binding error for a list or mapping
func mirrorSessionIDText(v any) (string, error) {
	if !anyutil.IsTrue(v) {
		return "", nil
	}
	switch n := v.(type) {
	case string:
		return n, nil
	case bool:
		return "1", nil
	case []any, *jsonutil.Object, map[string]any:
		return "", fmt.Errorf("the mirror lookup value is %s, which the database cannot match", errchain.Kind(n))
	}
	return anyutil.String(v), nil
}

// Shared session db registry acquire and release, swapped by tests
var (
	mirrorAcquireDB = state.Acquire
	mirrorReleaseDB = state.ReleaseOrClose
)

// Appends one role and content row to the session, returning the write failure
func appendMirrorMessageSQLite(ctx context.Context, sessionID, role, content string) error {
	db, err := mirrorAcquireDB(ctx, filepath.Join(home.Dir(ctx), "state.db"))
	if err != nil {
		return err
	}
	defer mirrorReleaseDB(db)
	_, err = db.AppendMessage(ctx, sessionID, map[string]any{"role": role, "content": content}, state.AppendGuards{})
	return err
}
