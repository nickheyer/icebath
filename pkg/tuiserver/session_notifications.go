package tuiserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Idle poll cadence kanban, loop, heartbeat and the bot mailbox share
const kanbanPollSeconds = 5.0

const (
	notifRefusalBackoffS    = 0.25
	notifRefusalBackoffMaxS = kanbanPollSeconds
)

// Raw and lookup session keys of a live session
func notifCurrentKeys(sid string, session *tuigateway.Session) map[string]bool {
	return map[string]bool{strOr(session.Get("session_key")): true, sessionLookupKey(session, sid): true}
}

// Whether a session answers to one of the keys
func notifSessionMatches(s *tuigateway.Session, keys map[string]bool) bool {
	return keys[strOr(s.Get("session_key"))] || keys[sessionLookupKey(s, "")]
}

// Whether any non finalized live session other than exclude answers to the keys
func notifLiveSessionMatches(keys map[string]bool, exclude *tuigateway.Session) bool {
	for _, live := range tuigateway.LiveSessions() {
		if live.Session != exclude && !truthy(live.Session.Get("_finalized")) && notifSessionMatches(live.Session, keys) {
			return true
		}
	}
	return false
}

// Continuation tip of a compression rotated key in the session's own store, else the key itself
func notifResolveEventKey(ctx context.Context, evtKey string, session *tuigateway.Session) (resolved string) {
	resolved = evtKey
	_ = catchPanic(func() {
		if session == nil {
			session = tuigateway.NewSession(nil)
		}
		db, release := sessionDB(ctx, session)
		defer release()
		if db == nil {
			return
		}
		if tip, err := db.ResolveResumeSessionID(evtKey); err == nil && tip != "" {
			resolved = tip
		}
	})
	return resolved
}

// Whether a different live session owns the event by UI origin or by its raw or resolved session key
func notificationEventBelongsElsewhere(ctx context.Context, sid string, session *tuigateway.Session, evt map[string]any) bool {
	evtUISID := strOr(evt["origin_ui_session_id"])
	if evtUISID != "" {
		if evtUISID == sid && !truthy(session.Get("_finalized")) {
			return false
		}
		if other := tuigateway.SessionRecord(evtUISID); other != nil && !truthy(other.Get("_finalized")) {
			return true
		}
	}
	evtKey := strOr(evt["session_key"])
	if evtKey == "" {
		return false
	}
	currentKeys := notifCurrentKeys(sid, session)
	resolvedKey := notifResolveEventKey(ctx, evtKey, session)
	if resolvedKey != evtKey {
		if currentKeys[resolvedKey] {
			return false
		}
		if notifLiveSessionMatches(map[string]bool{resolvedKey: true}, nil) {
			return true
		}
	}
	if currentKeys[evtKey] {
		return false
	}
	if resolvedKey == evtKey && notifOtherProfileSessionOwns(ctx, sid, session, evt) {
		return true
	}
	return notifLiveSessionMatches(map[string]bool{evtKey: true, resolvedKey: true}, session)
}

// Whether a live session on another profile store provably owns the event
func notifOtherProfileSessionOwns(ctx context.Context, sid string, session *tuigateway.Session, evt map[string]any) bool {
	ownHome := strOr(session.Get("profile_home"))
	for _, live := range tuigateway.LiveSessions() {
		other := live.Session
		if other == session || truthy(other.Get("_finalized")) || strOr(other.Get("profile_home")) == ownHome {
			continue
		}
		if sessionOwnsNotificationEvent(ctx, live.SID, other, evt) {
			return true
		}
	}
	return false
}

// Whether this session provably owns the event, the fail closed gate for addressed notifications
func sessionOwnsNotificationEvent(ctx context.Context, sid string, session *tuigateway.Session, evt map[string]any) bool {
	if truthy(session.Get("_finalized")) {
		return false
	}
	if evt["type"] == "async_delegation" && truthy(evt["startup_replay"]) &&
		!(tuigateway.SessionHasLiveTransport(session) || (tuigateway.StdioIsRPCChannel() && tuigateway.IsStdioTransport(session.Get("transport")))) {
		return false
	}
	if strOr(evt["origin_ui_session_id"]) == sid {
		return true
	}
	evtKey := strOr(evt["session_key"])
	currentKeys := notifCurrentKeys(sid, session)
	return evtKey != "" && (currentKeys[evtKey] || currentKeys[notifResolveEventKey(ctx, evtKey, session)])
}

// Whether an event must be positively claimed before TUI delivery
func notificationEventRequiresOwner(evt map[string]any) bool {
	return evt["type"] == "async_delegation" || truthy(evt["origin_ui_session_id"]) || truthy(evt["session_key"])
}

var dedupExtraFields = map[string][]string{
	"watch_match":     {"command", "pattern", "output", "suppressed", "message_id"},
	"heartbeat":       {"seq"},
	"watch_disabled":  {"command", "message", "suppressed"},
	"watch_overflow_": {"command", "message", "suppressed"},
}

// Value of an event field with a default like dict get
func evtGet(evt map[string]any, key string, def any) any {
	if v, ok := evt[key]; ok {
		return v
	}
	return def
}

// UI emission identity of a process notification event as the repr of its key tuple
func notificationEventDedupKey(evt map[string]any) string {
	evtType := pyrepr.Str(evtGet(evt, "type", "completion"))
	var key []any
	switch {
	case (evtType == "watch_match" || evtType == "watch_disabled") && truthy(evt["event_id"]):
		key = []any{evtGet(evt, "session_id", ""), evtType, evt["event_id"]}
	case evtType == "async_delegation":
		if truthy(evt["task_failure_notice"]) {
			first := map[string]any{}
			if results, ok := evt["results"].([]any); ok && len(results) > 0 {
				if r, isMap := results[0].(map[string]any); isMap && r != nil {
					first = r
				}
			} else if results, isMaps := evt["results"].([]map[string]any); isMaps && len(results) > 0 && results[0] != nil {
				first = results[0]
			}
			key = []any{evtGet(evt, "delegation_id", ""), evtType, "task_failure", evtGet(first, "task_index", "")}
		} else {
			key = []any{evtGet(evt, "delegation_id", ""), evtType}
		}
	default:
		lookup := evtType
		if strings.HasPrefix(evtType, "watch_overflow_") {
			lookup = "watch_overflow_"
		}
		key = []any{evtGet(evt, "session_id", ""), evtType}
		for _, f := range dedupExtraFields[lookup] {
			var def any = ""
			if f == "suppressed" {
				def = int64(0)
			}
			key = append(key, evtGet(evt, f, def))
		}
	}
	return pyrepr.Repr(pyrepr.Tuple(key))
}

// Releases the running claim of a notification turn
func notifReleaseTurn(session *tuigateway.Session) {
	unlock := lockHistory(session)
	session.Set("running", false)
	unlock()
}

// Claims the idle session for a notification turn, false when a turn is live or the user pressed Stop
func notifClaimTurn(session *tuigateway.Session) bool {
	claimed := false
	sessionTurnAdmission(session, func(admitted bool) {
		if !admitted || truthy(session.Get("running")) || truthy(session.Get("_turn_cancel_requested")) {
			return
		}
		session.Set("running", true)
		claimed = true
	})
	return claimed
}

// Reports a notification failure on stderr with its exception type
func notifLogFailure(what string, failure any) {
	fmt.Fprintf(os.Stderr, "[tui_gateway] %s: %s: %s\n", what, pyrepr.TypeName(failure), pyrepr.Str(failure))
}

// Parks an addressed event this session cannot prove it owns, deferring or dropping one the park refuses
func notifUnownedEvent(sid string, evt map[string]any, deferred *[]map[string]any) {
	evtType := pyrepr.Str(evtGet(evt, "type", "completion"))
	isDelegation := evt["type"] == "async_delegation"
	origin, key := strOr(evt["origin_ui_session_id"]), strOr(evt["session_key"])
	switch {
	case tuigateway.ParkNotification(evt):
		logger().Debug(fmt.Sprintf("Parked unowned %s notification (origin=%s key=%s) until its owner returns", evtType, pyrepr.Repr(origin), pyrepr.Repr(key)))
	case deferred != nil:
		if isDelegation {
			*deferred = append(*deferred, evt)
		} else {
			logger().Debug(fmt.Sprintf("Dropping unowned %s notification during shutdown drain (origin=%s key=%s)", evtType, pyrepr.Repr(origin), pyrepr.Repr(key)))
		}
	default:
		restored := isDelegation && truthy(evt["restored"])
		msg := fmt.Sprintf("Dropping unowned %s notification (origin=%s key=%s) instead of delivering to session %s", evtType, pyrepr.Repr(origin), pyrepr.Repr(key), sid)
		if isDelegation && !restored {
			logger().Warn(msg)
		} else {
			logger().Debug(msg)
		}
		if isDelegation {
			tools.ReturnCompletionOffer(evt)
		}
	}
}

// Waits before requeueing a refused notice, the session's next delay doubling to the poll period
func notifRefusalBackoff(session *tuigateway.Session) {
	delay := notifRefusalBackoffS
	if raw := session.Get("_notif_refusal_delay"); truthy(raw) {
		delay = floatOr(raw, notifRefusalBackoffS)
	}
	session.Set("_notif_refusal_delay", min(delay*2, notifRefusalBackoffMaxS))
	time.Sleep(time.Duration(delay * float64(time.Second)))
}

// Loop status row of a session
func notifLoopStatus(ctx context.Context, sid, text string) {
	tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "loop", "text": text})
}

// Defers a claimed delegation delivery and requeues the event
func notifDeferEvent(ctx context.Context, evt map[string]any, claim string, put func(map[string]any)) {
	if claim != "" {
		if _, err := tools.DeferCompletionDelivery(configEnv(ctx), pyrepr.Str(evt["delegation_id"]), claim); err != nil {
			panic(err)
		}
	}
	if put == nil {
		put = tools.DefaultProcessRegistry().CompletionQueue.Put
	}
	put(evt)
}

// Whether the owning profile set display.background_process_notifications off
func backgroundNotificationsOff(ctx context.Context, session *tuigateway.Session) bool {
	scoped := sessionProfileRuntimeScope(ctx, session, true)
	display, _ := loadCfg(scoped)["display"].(map[string]any)
	raw := display["background_process_notifications"]
	if raw == false {
		return true
	}
	text := ""
	if truthy(raw) {
		text = pyrepr.Str(raw)
	}
	return strings.ToLower(pystr.Strip(text)) == "off"
}

// Display only metadata of an async delegation completion
func asyncDelegationDisplayMetadata(evt map[string]any) map[string]any {
	var results []map[string]any
	switch raw := evt["results"].(type) {
	case []any:
		for _, r := range raw {
			if m, ok := r.(map[string]any); ok {
				results = append(results, m)
			}
		}
	case []map[string]any:
		results = append(results, raw...)
	}
	taskCount := len(results)
	if taskCount == 0 {
		taskCount = 1
	}
	completed, failed := 0, 0
	for _, r := range results {
		switch r["status"] {
		case "completed", "success":
			completed++
		case "failed", "error":
			failed++
		}
	}
	if completed == 0 {
		completed = taskCount - failed
	}
	meta := map[string]any{"display_text": tools.AsyncDelegationDisplayText(evt), "delegation_id": strOr(evt["delegation_id"]),
		"task_count": taskCount, "completed_count": completed, "failed_count": failed}
	duration := evt["total_duration_seconds"]
	if !truthy(duration) {
		duration = evt["duration_seconds"]
	}
	switch duration.(type) {
	case int, int64, float64, bool:
		meta["duration_seconds"] = duration
	}
	return meta
}

// Per surface note of this turn, the HUD prior or the spoken delegation contract, empty for the plain window
func hudSurfaceNote(ctx context.Context, session *tuigateway.Session) string {
	surface := session.Get("client_surface")
	a := sessionAgent(session)
	direct := map[string]bool{}
	if a != nil && a.ValidToolNames() != nil {
		direct = a.ValidToolNames()
	}
	if surface == "hud" && direct[tools.ToolCallName] {
		return agent.HudSurfaceNote(direct, a.ToolSearchScopedNames())
	}
	committed := pyjson.NewObject()
	committed.Set("surface", surface)
	committed.Set("voice_context", session.Get("voice_live_context"))
	return agent.SurfaceNote(committed, direct)
}

// Prefixes a per turn note onto the model input, leaving the prompt and the transcript alone
func prependNote(runMessage any, note string) any {
	if note == "" {
		return runMessage
	}
	switch m := runMessage.(type) {
	case string:
		return note + "\n\n" + m
	case []any:
		return append([]any{map[string]any{"type": "text", "text": note}}, m...)
	}
	return runMessage
}
