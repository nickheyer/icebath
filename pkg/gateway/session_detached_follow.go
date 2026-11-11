package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner classifying which conversation a background result belongs to, GatewayRunner._classify_completion_owner
type completionOwnerRunner interface {
	ClassifyCompletionOwner(ctx context.Context, parentSessionID, sessionKey string) (verdict, owner, target string, err error)
}

// Running key of the conversation the chat's route names through its routing entry
func chatRunningKey(ctx context.Context, runner AuthorityRunner, route string, entry *SessionEntry) (string, error) {
	a := activeAuthority(runner)
	if a == nil {
		return route, nil
	}
	owner, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return "", err
	}
	return runningKey(runner, route, owner), nil
}

// Boot schedules the queued input of conversations their chats moved past, returning the scheduled ids
func recoverDetachedSessions(ctx context.Context, a *SessionAuthority, pending map[string]bool) ([]string, error) {
	ids := make([]string, 0, len(pending))
	for sid := range pending {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	var scheduled []string
	for _, sid := range ids {
		if _, live := a.live(sid); live {
			continue
		}
		rows, err := a.DB.ListSessionAdmissions(ctx, sid, true)
		if err != nil {
			return nil, err
		}
		queued := false
		for _, row := range rows {
			if rowString(row, "status") == "queued" {
				queued = true
				break
			}
		}
		if !queued {
			continue
		}
		ref, err := restoreRoutedSession(ctx, a, sid)
		if err != nil {
			return nil, err
		}
		if ref == nil {
			continue
		}
		live, ok := a.live(sid)
		if !ok {
			return nil, &pyKeyError{sid}
		}
		if detachedRouteOf(live.Source) != nil {
			if err := scheduleSession(ctx, a, SessionRef{a.ProfileID, sid}); err != nil {
				return nil, err
			}
			scheduled = append(scheduled, sid)
		}
	}
	return scheduled, nil
}

// Background result moved onto its pinned conversation's detached key, else the event itself
func followDetachedPin(ctx context.Context, runner AuthorityRunner, evt map[string]any) (map[string]any, error) {
	key := ""
	if pyrepr.Truthy(evt["session_key"]) {
		key = pyrepr.Str(evt["session_key"])
	}
	pinned := ""
	if pyrepr.Truthy(evt["parent_session_id"]) {
		pinned = strings.TrimSpace(pyrepr.Str(evt["parent_session_id"]))
	}
	if key == "" || len(allAuthorities(runner)) == 0 {
		return evt, nil
	}
	if detached, ok := detachedSessionID(key); ok {
		return followDetachedKey(ctx, runner, evt, key, detached)
	}
	if pinned == "" {
		return evt, nil
	}
	store := runner.SessionStore()
	a, err := routeAuthority(runner, store, key)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return evt, nil
	}
	entry := store.LookupBySessionKey(ctx, key)
	if entry == nil {
		return evt, nil
	}
	classifier, ok := runner.(completionOwnerRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(runner) + "' object has no attribute '_classify_completion_owner'"}
	}
	verdict, _, target, err := classifier.ClassifyCompletionOwner(ctx, pinned, key)
	if err != nil {
		return nil, err
	}
	if verdict != "deliver" {
		return evt, nil
	}
	sessionID, err := a.LogicalOwner(ctx, target)
	if err != nil {
		return nil, err
	}
	current, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return nil, err
	}
	if sessionID == current {
		return evt, nil
	}
	row, err := a.DB.GetSession(ctx, target)
	if err != nil {
		return nil, err
	}
	if row == nil || pyrepr.Truthy(row.GetOr("ended_at", nil)) {
		return evt, nil
	}
	live, err := mountedSession(ctx, a, sessionID)
	if err != nil {
		return nil, err
	}
	if live == nil || live.Route != detachedKey(key, sessionID) {
		return evt, nil
	}
	return withSessionKey(evt, live.Route), nil
}

// Copy of an event naming another session key
func withSessionKey(evt map[string]any, key string) map[string]any {
	out := make(map[string]any, len(evt))
	for k, v := range evt {
		out[k] = v
	}
	out["session_key"] = key
	return out
}

// Detached key result moved onto the chat's route once the chat names its conversation again
func followDetachedKey(ctx context.Context, runner AuthorityRunner, evt map[string]any, key, sessionID string) (map[string]any, error) {
	a, err := routeAuthority(runner, runner.SessionStore(), key)
	if err != nil {
		return nil, err
	}
	var live *LiveSession
	if a != nil {
		if live, err = mountedSession(ctx, a, sessionID); err != nil {
			return nil, err
		}
	}
	if live == nil || live.Route == key {
		return evt, nil
	}
	return withSessionKey(evt, live.Route), nil
}

// Live session of a conversation mounted as its route would be after a restart, nil while draining
func mountedSession(ctx context.Context, a *SessionAuthority, sessionID string) (*LiveSession, error) {
	if _, live := a.live(sessionID); !live {
		if _, err := restoreRoutedSession(ownerScope(ctx, a, false), a, sessionID); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				return nil, nil
			}
			return nil, err
		}
	}
	live, _ := a.live(sessionID)
	return live, nil
}

// Background result on its pinned conversation's detached key, for the runner's notification code
func FollowDetachedPin(ctx context.Context, runner AuthorityRunner, evt map[string]any) (map[string]any, error) {
	return followDetachedPin(ctx, runner, evt)
}

// Running key of the conversation the chat's route names, for the runner's stop paths
func ChatRunningKey(ctx context.Context, runner AuthorityRunner, route string, entry *SessionEntry) (string, error) {
	return chatRunningKey(ctx, runner, route, entry)
}

// Boot schedule of detached conversations' queued input, for the runner's startup recovery
func RecoverDetachedSessions(ctx context.Context, a *SessionAuthority, pending map[string]bool) ([]string, error) {
	return recoverDetachedSessions(ctx, a, pending)
}
