package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
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
			slog.Warn(fmt.Sprintf("Mirror failed for %s:%s thread=%s user=%s session=None: %v", platform, chatID,
				optText(opts.ThreadID), optText(opts.UserID), err))
			return false
		}
		sessionID = found
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

// Active session of a platform chat from state.db, then the pre migration sessions.json scan, the scan's Python errors returned
func findMirrorSessionID(ctx context.Context, platform, chatID string, threadID, userID *string) (string, error) {
	if id, err := func() (string, error) {
		db, err := mirrorAcquireDB(ctx, filepath.Join(home.Dir(ctx), "state.db"))
		if err != nil {
			return "", err
		}
		defer mirrorReleaseDB(db)
		return state.FindSessionByOrigin(ctx, db, platform, chatID, threadID, userID)
	}(); err != nil {
		slog.Debug(fmt.Sprintf("Mirror state.db session lookup failed: %v", err))
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
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return "", nil
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return "", nil
	}
	data, isObj := parsed.(*pyjson.Object)
	if !isObj {
		return "", mirrorAttributeError(parsed, "items")
	}
	var candidates []*pyjson.Object
	for _, k := range data.Keys {
		entry, ok := data.Vals[k].(*pyjson.Object)
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
		var exact []*pyjson.Object
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
			if u := pystr.Strip(originUserID(e)); u != "" {
				users[u] = true
			}
		}
		if len(users) > 1 {
			return "", nil
		}
	}
	best, bestKey := candidates[0], updatedAt(candidates[0])
	for _, e := range candidates[1:] {
		key := updatedAt(e)
		greater, err := mirrorPyGreater(key, bestKey)
		if err != nil {
			return "", err
		}
		if greater {
			best, bestKey = e, key
		}
	}
	return mirrorSessionIDText(best.Vals["session_id"])
}

// AttributeError Python raises reading a missing attribute of a decoded JSON value
func mirrorAttributeError(v any, attr string) error {
	return fmt.Errorf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), attr)
}

// Whether an index entry's origin names the platform, chat and thread, with the AttributeError a non mapping origin raises
func mirrorEntryMatches(entry *pyjson.Object, platform, chatID string, threadID *string) (bool, error) {
	originValue := entry.Vals["origin"]
	if !pyrepr.Truthy(originValue) {
		originValue = pyjson.NewObject()
	}
	origin, isObj := originValue.(*pyjson.Object)
	if !isObj {
		return false, mirrorAttributeError(originValue, "get")
	}
	plat := origin.Vals["platform"]
	if !pyrepr.Truthy(plat) {
		plat = ""
		if v, ok := entry.Get("platform"); ok {
			plat = v
		}
	}
	platText, isStr := plat.(string)
	if !isStr {
		return false, mirrorAttributeError(plat, "lower")
	}
	if pystr.Lower(platText) != pystr.Lower(platform) {
		return false, nil
	}
	chat, ok := origin.Get("chat_id")
	if !ok {
		chat = ""
	}
	if pyrepr.Str(chat) != chatID {
		return false, nil
	}
	if threadID == nil {
		return true, nil
	}
	thread := origin.Vals["thread_id"]
	if !pyrepr.Truthy(thread) {
		thread = ""
	}
	return pyrepr.Str(thread) == *threadID, nil
}

// updated_at of an index entry, empty when missing
func updatedAt(e *pyjson.Object) any {
	if v, ok := e.Get("updated_at"); ok {
		return v
	}
	return ""
}

// Python a > b over decoded JSON values, numbers and strings and lists ordered, the TypeError for any other pairing
func mirrorPyGreater(a, b any) (bool, error) {
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			return sa > sb, nil
		}
	}
	if na, ok := mirrorPyNumber(a); ok {
		if nb, ok := mirrorPyNumber(b); ok {
			return na != nil && nb != nil && na.Cmp(nb) > 0, nil
		}
	}
	if la, ok := a.([]any); ok {
		if lb, ok := b.([]any); ok {
			for i := 0; i < len(la) && i < len(lb); i++ {
				if !pyjson.Equal(la[i], lb[i]) {
					return mirrorPyGreater(la[i], lb[i])
				}
			}
			return len(la) > len(lb), nil
		}
	}
	return false, fmt.Errorf("'>' not supported between instances of '%s' and '%s'", pyrepr.TypeName(a), pyrepr.TypeName(b))
}

// Numeric value of a Python bool, int or float, nil standing for NaN which orders against nothing
func mirrorPyNumber(v any) (*big.Float, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case int:
		return new(big.Float).SetInt64(int64(n)), true
	case int64:
		return new(big.Float).SetInt64(n), true
	case *big.Int:
		return new(big.Float).SetPrec(uint(n.BitLen()) + 64).SetInt(n), true
	case float64:
		if math.IsNaN(n) {
			return nil, true
		}
		return new(big.Float).SetPrec(64).SetFloat64(n), true
	}
	return nil, false
}

// Session id an index entry holds as bound text, empty when falsy, the binding error for a list or mapping
func mirrorSessionIDText(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	switch n := v.(type) {
	case string:
		return n, nil
	case bool:
		return "1", nil
	case []any, *pyjson.Object, map[string]any:
		return "", fmt.Errorf("Error binding parameter 1: type '%s' is not supported", pyrepr.TypeName(n))
	}
	return pyrepr.Str(v), nil
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
