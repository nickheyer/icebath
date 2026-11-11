package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Stored title of a live session, else its pending title
func sessionLiveTitle(ctx context.Context, session *tuigateway.Session, key string) (title string) {
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
func sessionPendingKind(sid string) string { return tuigateway.PendingKind(sid) }

// Live status of a session, waiting on a request, starting its agent, working a turn or idle
func sessionLiveStatus(sid string, session *tuigateway.Session) string {
	if sessionPendingKind(sid) != "" {
		return "waiting"
	}
	if ready, isEvent := session.Get("agent_ready").(*pythread.Event); isEvent && ready != nil && !ready.IsSet() && truthy(session.Get("agent_build_started")) {
		return "starting"
	}
	if truthy(session.Get("running")) {
		return "working"
	}
	return "idle"
}

// Stored session id of a live record, its agent's id first, then its key, then the fallback
func sessionLookupKey(session *tuigateway.Session, fallback string) string {
	if a := sessionAgent(session); a != nil && a.SessionID() != "" {
		return a.SessionID()
	}
	if key := strOr(session.Get("session_key")); key != "" {
		return key
	}
	return fallback
}

// Whitespace collapsed text cut to 160 characters
func livePreview(text string) string {
	return pystr.Slice(strings.Join(pystr.SplitWhitespace(text, -1), " "), 0, 160)
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

// Live session row of the session pickers, its status, preview, model and stored title
func sessionLiveItem(ctx context.Context, sid string, session *tuigateway.Session, currentSID string) map[string]any {
	key := sessionLookupKey(session, sid)
	a := sessionAgent(session)
	history, _ := session.Get("history").([]any)
	status := sessionLiveStatus(sid, session)
	inflight, queued := inflightSnapshot(session), queuedPromptSnapshot(session)
	preview := ""
	for i := len(history) - 1; i >= 0; i-- {
		msg, _ := history[i].(map[string]any)
		content, hasContent := msg["content"]
		if !hasContent {
			content = msg["text"]
			if _, hasText := msg["text"]; !hasText {
				content = ""
			}
		}
		if text := pystr.Strip(tuigateway.ContentDisplayText(content)); text != "" {
			preview = livePreview(text)
			break
		}
	}
	if queued != nil {
		text := preview
		if truthy(queued["user"]) {
			text = strOr(queued["user"])
		}
		preview = livePreview(text)
	} else if inflight != nil {
		text := preview
		if truthy(inflight["assistant"]) {
			text = strOr(inflight["assistant"])
		} else if truthy(inflight["user"]) {
			text = strOr(inflight["user"])
		}
		preview = livePreview(text)
	}
	now := nowSeconds()
	model := ""
	if a != nil {
		model = a.Model()
	}
	if model == "" {
		model = resolveModel(ctx)
	}
	lastActive := session.Get("last_active")
	if !truthy(lastActive) {
		lastActive = session.Get("created_at")
	}
	return map[string]any{
		"current": sid == currentSID, "id": sid,
		"last_active":   floatOr(lastActive, now),
		"message_count": len(history),
		"model":         model, "preview": preview,
		"session_key": key, "started_at": floatOr(session.Get("created_at"), now), "status": status,
		"title": sessionLiveTitle(ctx, session, key),
	}
}
