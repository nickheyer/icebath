package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Raised when a rewind target or its retry text is invalid, the Python ValueError of the rewind
type rewindValueError struct{ msg string }

func (e *rewindValueError) Error() string      { return e.msg }
func (e *rewindValueError) PyTypeName() string { return "ValueError" }

// Durable transcript copies without the transient recovery rows
func historyWithoutEphemeralScaffolding(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, entry := range history {
		if state.IsEphemeralScaffolding(entry) {
			continue
		}
		if m, isDict := entry.(map[string]any); isDict {
			out = append(out, copyDict(m))
		}
	}
	return out
}

// Rewinds one canonical user turn keeping carrier scaffolding, through the store when the session persists
func rewindActiveSessionHistory(ctx context.Context, session *tuigateway.Session, userOrdinal int, requireRetryable bool) ([]map[string]any, map[string]any, int, error) {
	history := historyWithoutEphemeralScaffolding(historyList(session.Get("history")))
	var userIndices []int
	for i, m := range history {
		if state.UserOriginatedTurnView(m) != nil {
			userIndices = append(userIndices, i)
		}
	}
	if userOrdinal < 0 || userOrdinal >= len(userIndices) {
		return nil, nil, 0, &rewindValueError{"target user message is no longer in session history"}
	}
	sessionKey := pystr.Strip(strOr(session.Get("session_key")))
	var installed []map[string]any
	var liveView map[string]any
	var rewoundCount int
	if sessionKey != "" {
		db, release := sessionDB(ctx, session)
		outcome, err := func() (*state.RewindOutcome, error) {
			defer release()
			if db == nil {
				return nil, errors.New("session database is unavailable")
			}
			return db.RewindUserTurn(ctx, sessionKey, userOrdinal, state.RewindUserTurnOptions{WarmHistory: history,
				HasWarmHistory: true, RequireRetryable: requireRetryable, AdoptRowIDs: true})
		}()
		if err != nil {
			return nil, nil, 0, err
		}
		installed, liveView, rewoundCount = outcome.Prefix, outcome.LiveView, outcome.RewoundCount
	} else {
		targetIndex := userIndices[userOrdinal]
		prefix, view, err := state.HistoryBeforeUserOriginatedTurn(history, targetIndex)
		if err != nil {
			return nil, nil, 0, err
		}
		installed, liveView, rewoundCount = prefix, view, len(history)-targetIndex
		if requireRetryable {
			if _, err := state.RetryableUserText(liveView["content"]); err != nil {
				return nil, nil, 0, err
			}
		}
	}
	copied := make([]map[string]any, len(installed))
	for i, message := range installed {
		copied[i] = copyDict(message)
	}
	session.Set("history", messagesAsHistory(copied))
	session.Set("history_version", intOr(session.Get("history_version"))+1)
	if a := sessionAgent(session); a != nil {
		a.InstallRewoundHistory(copied, sessionKey != "")
	}
	return copied, liveView, rewoundCount, nil
}
