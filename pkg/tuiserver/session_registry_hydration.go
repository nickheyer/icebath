package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Plain rows of a stored transcript as history entries
func anyRows(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Raw, display and ancestor prefix of a cold resume, the tip alone once the lineage passes the resume limit
func loadResumeTranscript(ctx context.Context, db *state.DB, storedID string, modelHistoryOnly bool) ([]map[string]any, []map[string]any, []map[string]any, error) {
	if modelHistoryOnly {
		raw, err := db.GetMessagesAsConversation(ctx, storedID, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
		return raw, []map[string]any{}, []map[string]any{}, err
	}
	prefixFits := true
	if _, err := db.AssertResumeSafe(ctx, storedID, nil, false); err != nil {
		var tooLarge *state.SessionResumeTooLargeError
		if errors.As(err, &tooLarge) {
			prefixFits = false
			logger().Info(fmt.Sprintf("resume %s: compression lineage exceeds the resume limit (%s); hydrating the tip segment only", storedID, err))
		} else {
			logger().Debug("resume lineage guard failed; loading full lineage", "exc_info", err.Error())
		}
	}
	if prefixFits {
		raw, display, err := db.GetResumeConversations(ctx, storedID)
		if err != nil {
			return nil, nil, nil, err
		}
		prefix, err := db.GetAncestorDisplayPrefix(ctx, storedID)
		return raw, display, prefix, err
	}
	raw, err := db.GetMessagesAsConversation(ctx, storedID, state.ConversationOptions{RepairAlternation: true, IncludeRowIDs: true})
	return raw, raw, []map[string]any{}, err
}

// Loads a deferred resume's transcript off the response path, then schedules auto continue and the agent build
func scheduleResumeHydration(ctx context.Context, sid, storedID string, db *state.DB, closeDB, modelHistoryOnly bool) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer threadExceptHook("resume-hydrate-" + sid)
		session := tuigateway.SessionRecord(sid)
		if session == nil {
			if closeDB {
				suppress(func() { _ = db.Close() })
			}
			return
		}
		defer func() {
			if closeDB {
				if err := db.Close(); err != nil {
					logger().Debug(fmt.Sprintf("failed to close resume db for %s", sid), "exc_info", err.Error())
				}
			}
		}()
		failure := catchPanic(func() {
			tuigateway.Emit(ctx, "session.resume_progress", sid, map[string]any{"phase": "history", "status": "loading"})
			_, err := db.RetireUndrainedQueueRows(ctx, storedID)
			panicIf(err)
			raw, display, prefix, err := loadResumeTranscript(ctx, db, storedID, modelHistoryOnly)
			panicIf(err)
			history := anyRows(agent.CanonicalizeReplayHistory(raw, nil))
			if tuigateway.SessionRecord(sid) != session {
				return
			}
			unlock := lockHistory(session)
			session.Set("history", history)
			session.Set("display_history_prefix", anyRows(prefix))
			session.Set("resume_hydrating", false)
			if !modelHistoryOnly {
				session.Set("resume_message_count", len(display))
			}
			unlock()
			if todo := tuigateway.TodoStateFromHistory(historyDicts(history)); todo != nil && session.Get("todo_state") == nil {
				session.Set("todo_state", todo)
			}
			if ready, isEvent := session.Get("resume_history_ready").(*pythread.Event); isEvent {
				ready.Set()
			}
			tuigateway.Emit(ctx, "session.resume_progress", sid, map[string]any{"message_count": session.Get("resume_message_count"),
				"phase": "history", "status": "complete"})
			maybeScheduleAutoContinue(ctx, sid, session, storedID)
			startAgentBuild(ctx, sid, session)
		})
		if failure == nil || tuigateway.SessionRecord(sid) != session {
			return
		}
		message := resumeFailedMessage(failure)
		session.Set("resume_hydrating", false)
		session.Set("resume_history_error", message)
		session.Set("agent_error", message)
		for _, key := range []string{"resume_history_ready", "agent_ready"} {
			if ready, isEvent := session.Get(key).(*pythread.Event); isEvent {
				ready.Set()
			}
		}
		tuigateway.Emit(ctx, "session.resume_progress", sid, map[string]any{"message": message, "phase": "history", "status": "failed"})
		tuigateway.Emit(ctx, "error", sid, map[string]any{"message": message})
		var discarded *tuigateway.Session
		func() {
			sessionsLock.Lock()
			defer sessionsLock.Unlock()
			if tuigateway.SessionRecord(sid) == session {
				discarded = session
				tuigateway.SetSessionRecord(sid, nil)
			}
		}()
		if discarded != nil {
			if lease, held := discarded.Get("active_session_lease").(*activesessions.Lease); held && lease != nil {
				panicIf(lease.Release())
			}
		}
	}()
}
