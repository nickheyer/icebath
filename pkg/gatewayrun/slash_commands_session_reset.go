package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bound on the old agent teardown during a reset before the reset proceeds without it
const resetCleanupTimeoutS = 30.0

// Random tip in the active language, assigned in cmd/icebath wiring
var RandomTip func(ctx context.Context) string

// Whether the error is a Python ValueError or one of its subclasses
func isValueError(err error) bool {
	var pe *pyint.Error
	if errors.As(err, &pe) && pe.Kind == "ValueError" {
		return true
	}
	var unavailable *state.RewindTargetUnavailableError
	if errors.As(err, &unavailable) {
		return true
	}
	return pyrepr.ExceptionName(err) == "ValueError"
}

// Drops env passthrough and credential file state at a conversation boundary
func resetProcessScopedToolState(ctx context.Context) {
	tools.ClearEnvPassthrough()
	tools.ClearCredentialFiles(slashToolsEnv(ctx))
}

// Acknowledges every turn of the session lost in a restart through the owner, reporting how many resolved
func discardLostTurns(ctx context.Context, authority *gateway.SessionAuthority, sessionID string) (string, error) {
	rows, err := state.ListSessionAdmissions(ctx, authority.DB, sessionID, false)
	if err != nil {
		return "", err
	}
	var lost []*pyjson.Object
	for _, row := range rows {
		if status, _ := row.Get("status"); status == "unknown" {
			lost = append(lost, row)
		}
	}
	if len(lost) == 0 {
		return i18n.T(ctx, "gateway.discard.nothing", "", nil), nil
	}
	ref := gateway.SessionRef{ProfileID: authority.ProfileID, SessionID: sessionID}
	done := 0
	for _, row := range lost {
		admissionID, _ := row.Get("admission_id")
		generationValue, _ := row.Get("generation")
		generation, err := pyint.Int64(generationValue)
		if err != nil {
			return "", err
		}
		if _, err := authority.ResolveUnknownAsOwner(ctx, ref, pyrepr.Str(admissionID), generation, nil); err != nil {
			var refused *state.RuntimeStoreError
			if !errors.As(err, &refused) {
				return "", err
			}
			runLog().Warn(fmt.Sprintf("/discard of %s refused: %s", pyrepr.Str(admissionID), refused.Reason))
			continue
		}
		done++
	}
	if done == 0 {
		return i18n.T(ctx, "gateway.discard.nothing", "", nil), nil
	}
	return i18n.T(ctx, "gateway.discard.done", "", map[string]any{"count": done}), nil
}

// Closes the old agent's tool resources off the loop, proceeding after the bound while the worker finishes
func (r *GatewayRunner) cleanupOldAgentForReset(ctx context.Context, sessionKey string) {
	oldAgent := r.cachedAgentFor(sessionKey, false)
	if oldAgent == nil {
		return
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.runHousekeepingInExecutor(ctx, func(workerCtx context.Context) (any, error) {
			r.cleanupAgentResources(workerCtx, oldAgent)
			return nil, nil
		})
		done <- err
	}()
	timer := time.NewTimer(time.Duration(resetCleanupTimeoutS * float64(time.Second)))
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			runLog().Warn(fmt.Sprintf("Agent resource cleanup for session %s failed during /new reset: %s (#35994)",
				sessionKey, pyrepr.ExceptionMessage(err)))
		}
	case <-timer.C:
		runLog().Warn(fmt.Sprintf("Agent resource cleanup for session %s exceeded %ss during /new reset; proceeding with "+
			"reset (the worker thread is left to finish on its own). (#35994)", sessionKey, pyrepr.Float(resetCleanupTimeoutS)))
	}
}

// Runs plugin finalize off the loop, then the session end and session reset hooks
func (r *GatewayRunner) fireSessionResetHooks(ctx context.Context, source *gateway.SessionSource, sessionKey string, oldSID, newSID any) error {
	platformValue := string(source.Platform)
	func() {
		defer func() { _ = recover() }()
		messages := r.finalizeSessionOffLoop(ctx, oldSID, platformValue, "new_session", "",
			map[string]any{"old_session_id": oldSID, "new_session_id": newSID})
		_ = r.deliverSessionEndMessages(ctx, messages, source, sessionKey)
	}()
	var userID any
	if source.UserID != nil {
		userID = *source.UserID
	}
	payload := func() map[string]any {
		return map[string]any{"platform": platformValue, "user_id": userID, "session_key": sessionKey}
	}
	if err := r.hooks.emit(ctx, "session:end", payload()); err != nil {
		return err
	}
	return r.hooks.emit(ctx, "session:reset", payload())
}

// Drops the chat's turn lost in a gateway restart so its queue runs again
func (r *GatewayRunner) handleDiscardCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	authorized, err := r.isUserAuthorizedForSource(ctx, event.Source, false)
	if err != nil {
		return "", err
	}
	if !authorized {
		return i18n.T(ctx, "gateway.discard.nothing", "", nil), nil
	}
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return "", err
	}
	sessionKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return "", err
	}
	sid := ""
	if authority != nil {
		peeked, _ := r.sessionStore.PeekSessionID(sessionKey)
		if sid, err = authority.LogicalOwner(ctx, peeked); err != nil {
			return "", err
		}
	}
	if sid == "" {
		return i18n.T(ctx, "gateway.discard.nothing", "", nil), nil
	}
	if !authority.HasLiveSession(sid) {
		if _, err := authority.PublishRoute(ctx, event.Source, sid, sessionKey); err != nil {
			return "", err
		}
	}
	return discardLostTurns(ctx, authority, sid)
}

// Starts a fresh session for the chat, tearing down the old agent and conversation scoped state first
func (r *GatewayRunner) handleResetCommand(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	source := event.Source
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := r.hmStopOwnerTurn(ctx, sessionKey); err != nil {
		return nil, err
	}
	r.invalidateSessionRunGeneration(ctx, sessionKey, "session_reset")
	r.releaseRunningAgentState(ctx, sessionKey, nil)
	oldEntry := r.sessionStore.LoadedEntry(ctx, sessionKey)
	r.cleanupOldAgentForReset(ctx, sessionKey)
	r.evictCachedAgent(ctx, sessionKey, false, false)
	r.clearConversationScope(ctx, sessionKey, "session_reset")
	var oldSID any
	oldSIDText := ""
	if oldEntry != nil {
		oldSID, oldSIDText = oldEntry.SessionID, oldEntry.SessionID
	}
	func() {
		defer func() { _ = recover() }()
		tools.InterruptAsyncDelegationsForSession(sessionKey, "", oldSIDText, "session_reset")
	}()
	resetProcessScopedToolState(ctx)

	lock, err := gateway.RouteOwnerLock(ctx, r, oldSIDText)
	if err != nil {
		return nil, err
	}
	newEntry, err := func() (*gateway.SessionEntry, error) {
		lock.Lock()
		defer lock.Unlock()
		entry, err := r.sessionStore.ResetSession(ctx, sessionKey, nil)
		if err != nil {
			return nil, err
		}
		if err := gateway.MoveRouteWakes(ctx, r, sessionKey, oldSIDText); err != nil {
			return nil, err
		}
		return entry, nil
	}()
	if err != nil {
		return nil, err
	}
	var newSID any
	if newEntry != nil {
		newSID = newEntry.SessionID
	}
	if err := r.fireSessionResetHooks(ctx, source, sessionKey, oldSID, newSID); err != nil {
		return nil, err
	}
	sessionInfo, err := r.resetNoticeSessionInfo(ctx, source)
	if err != nil {
		sessionInfo = ""
	}
	var defaultHeader string
	if newEntry != nil {
		defaultHeader = i18n.T(ctx, "gateway.reset.header_default", "", nil)
	} else {
		if newEntry, err = r.sessionStore.GetOrCreateSession(ctx, source, true, true); err != nil {
			return nil, err
		}
		defaultHeader = i18n.T(ctx, "gateway.reset.header_new", "", nil)
	}
	header, ok := r.telegramTopicNewHeader(ctx, source)
	if !ok || header == "" {
		header = defaultHeader
	}
	titleArg := pystr.Strip(event.GetCommandArgs())
	if titleArg != "" && r.sessionDB(ctx) != nil && newEntry != nil {
		header = r.resetTitledHeader(ctx, header, newEntry.SessionID, titleArg)
	}
	if r.isTelegramTopicLane(ctx, source) && newEntry != nil {
		if err := r.recordTelegramTopicBinding(ctx, source, newEntry); err != nil {
			runLog().Debug("Failed to rebind Telegram topic after /new", "exc_info", err)
		}
	}
	var newSIDValue any
	if newEntry != nil {
		newSIDValue = newEntry.SessionID
	}
	func() {
		defer func() { _ = recover() }()
		agent.LifecycleInvokeHook(ctx, "on_session_reset", map[string]any{"session_id": newSIDValue, "reason": "new_session",
			"platform": string(source.Platform), "old_session_id": oldSID, "new_session_id": newSIDValue})
	}()
	tipLine := func() (line string) {
		defer func() {
			if recover() != nil {
				line = ""
			}
		}()
		return i18n.T(ctx, "gateway.reset.tip", "", map[string]any{"tip": RandomTip(ctx)})
	}()
	body := header
	if sessionInfo != "" {
		body = header + "\n\n" + sessionInfo
	}
	return gateway.NewEphemeralReply(body+tipLine, nil), nil
}

// Titled header for a titled reset on success, else the header with the rejection note
func (r *GatewayRunner) resetTitledHeader(ctx context.Context, header, sessionID, titleArg string) string {
	note := ""
	sanitized, err := state.SanitizeTitle(titleArg)
	if err != nil {
		sanitized = ""
		note = i18n.T(ctx, "gateway.reset.title_rejected", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})
	}
	if sanitized != "" {
		db := r.sessionDB(ctx)
		_, err := state.AsyncCall(ctx, db, func(d *state.SessionDB) (bool, error) { return d.SetSessionTitle(ctx, sessionID, sanitized) })
		switch {
		case err == nil:
			header = i18n.T(ctx, "gateway.reset.header_titled", "", map[string]any{"title": sanitized})
		case isValueError(err):
			note = i18n.T(ctx, "gateway.reset.title_error_untitled", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})
		}
	} else if note == "" {
		note = i18n.T(ctx, "gateway.reset.title_empty_untitled", "", nil)
	}
	return header + note
}
