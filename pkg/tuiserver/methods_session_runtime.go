package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Status report facts and their display lines, assigned from the CLI status report at startup
var StatusReport struct {
	Fields func(ctx context.Context, sessionID string, a *agent.AIAgent, meta map[string]any, model, provider string, tokens any, agentRunning bool, profileHome string) map[string]any
	Lines  func(fields map[string]any, keys ...string) []string
}

func registerSessionRuntimeHandlers() {
	sessionMethod("session.status", false, sessionStatus)
	sessionMethod("session.history", false, sessionHistory)
	sessionMethod("session.undo", true, sessionUndo)
	sessionRegistry.scopedMethod("session.compress", sessionCompress)
	sessionMethod("session.save", true, sessionSave)
	sessionRegistry.method("session.close", sessionClose)
	sessionMethod("terminal.resize", false, terminalResize)
}

// Stored row of the key from the live session's bound profile db first, else params profile or launch
func statusRow(ctx context.Context, session *tuihost.Session, params map[string]any, key string) map[string]any {
	if key == "" {
		return map[string]any{}
	}
	db, release := sessionDB(ctx, session)
	defer release()
	if db != nil {
		return tryGetSession(ctx, db, key)
	}
	db2, release2, err := profileDB(ctx, params, false)
	if err != nil {
		panic(err)
	}
	defer release2()
	if db2 == nil {
		return map[string]any{}
	}
	return tryGetSession(ctx, db2, key)
}

// Session row as a dict, empty when it is absent or the read fails
func tryGetSession(ctx context.Context, db *state.DB, key string) map[string]any {
	var out map[string]any
	if failure := catchPanic(func() {
		row, err := db.GetSession(ctx, key)
		panicIf(err)
		if row != nil {
			out, _ = pyjson.Plain(row).(map[string]any)
		}
	}); failure != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// Status text of a session from its live agent
func sessionStatus(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
	key := strOr(session.Get("session_key"))
	if key == "" {
		key = strOr(params["session_id"])
	}
	a := sessionAgent(session)
	model, providerName := "", ""
	if a != nil {
		model, providerName = a.Model(), a.Provider()
	}
	fields := StatusReport.Fields(ctx, key, a, statusRow(ctx, session, params, key), model, providerName,
		sessionUsageSnapshot(ctx, session)["total"], truthy(session.Get("running")), strOr(session.Get("profile_home")))
	lines := []string{"Icebath TUI Status", ""}
	lines = append(lines, StatusReport.Lines(fields, "session_id", "path")...)
	if project, isDict := projectInfoForCwd(ctx, displaySessionCwd(ctx, session)).(map[string]any); isDict && project != nil {
		lines = append(lines, "Project: "+pyrepr.Str(project["name"]))
	}
	lines = append(lines, StatusReport.Lines(fields, "title", "model", "created", "last_activity", "tokens", "agent_running")...)
	return ok(rid, map[string]any{"output": strings.Join(lines, "\n")})
}

// Transcript of the session the live agent writes to with durable row ids, the live history when unreadable
func sessionHistory(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
	history := append([]any{}, historyList(session.Get("history"))...)
	if rowSession := submitRowTargetKey(session); rowSession != "" {
		db, release := sessionDB(ctx, session)
		func() {
			defer release()
			if db == nil {
				return
			}
			suppress(func() {
				rows, err := db.GetMessagesAsConversation(ctx, rowSession, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true})
				panicIf(err)
				history = messagesAsHistory(rows)
			})
		}()
	}
	messages, err := tuihost.HistoryToMessages(ctx, history, strOr(session.Get("profile_home")), false)
	panicIf(err)
	return ok(rid, map[string]any{"count": len(history), "messages": messages})
}

// Drops the last real user turn and what followed, refused while a turn runs
func sessionUndo(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
	busy := rpcErr(rid, 4009, busyMessage("undo", truthy(session.Get("_manual_compress_active"))), nil)
	if truthy(session.Get("running")) {
		return busy
	}
	removed := 0
	unlock := lockHistory(session)
	if truthy(session.Get("running")) {
		unlock()
		return busy
	}
	history := historyWithoutEphemeralScaffolding(historyList(session.Get("history")))
	userTurns := 0
	for _, message := range history {
		if agent.UserOriginatedTurnView(message) != nil {
			userTurns++
		}
	}
	if userTurns > 0 {
		var rewindErr error
		if failure := catchPanic(func() {
			_, _, removed, rewindErr = rewindActiveSessionHistory(ctx, session, userTurns-1, false)
		}); failure != nil {
			rewindErr = fmt.Errorf("%s", pyrepr.Str(failure))
		}
		if rewindErr != nil {
			unlock()
			return rpcErr(rid, 5008, "undo: "+pyrepr.Str(rewindErr), nil)
		}
	}
	unlock()
	return ok(rid, map[string]any{"removed": removed})
}

// In process session.compress pinning the compressing status, then the before and after summary with messages
func compressLive(ctx context.Context, rid any, sid string, session *tuihost.Session, focusTopic string) map[string]any {
	unlock := lockHistory(session)
	beforeMessages := append([]any{}, historyList(session.Get("history"))...)
	historyVersion := intOr(session.Get("history_version"))
	unlock()
	beforeCount := len(beforeMessages)
	tokens := func(msgs []any) int {
		if len(msgs) == 0 {
			return 0
		}
		return agent.EstimateManualRequestTokens(ctx, sessionAgent(session), historyAsMessages(msgs))
	}
	beforeTokens := tokens(beforeMessages)
	if beforeCount >= 4 {
		focusSuffix := ""
		if focusTopic != "" {
			focusSuffix = fmt.Sprintf(", focus: \"%s\"", focusTopic)
		}
		text := fmt.Sprintf("⠋ compressing %d messages (~%s tok)%s…", beforeCount, pystr.GroupThousands(int64(beforeTokens)), focusSuffix)
		statusUpdate(ctx, sid, "compressing", &text)
	}
	defer statusUpdate(ctx, sid, "ready", nil)
	removed, usage, err := compressSessionHistory(ctx, session, focusTopic, beforeMessages, &historyVersion)
	panicIf(err)
	unlock = lockHistory(session)
	messages := append([]any{}, historyList(session.Get("history"))...)
	unlock()
	afterTokens := tokens(messages)
	a := sessionAgent(session)
	syncSessionKeyAfterCompress(ctx, sid, session, true, true)
	var engine any
	if a != nil {
		engine = a.ContextCompressor()
	}
	summary := agent.SessionCompressAgent{}.ManualSummary(historyAsMessages(beforeMessages), historyAsMessages(messages), beforeTokens, afterTokens, engine)
	info := sessionInfo(ctx, a, session)
	tuihost.Emit(ctx, "session.info", sid, info)
	agent.FinalizeContextEngineCompressionNotification(a, true)
	status := "compressed"
	if summary["aborted"] == true {
		status = "aborted"
	}
	wire, convErr := tuihost.HistoryToMessages(ctx, messages, strOr(session.Get("profile_home")), false)
	panicIf(convErr)
	return ok(rid, map[string]any{"status": status, "removed": removed, "before_messages": beforeCount, "after_messages": len(messages),
		"before_tokens": beforeTokens, "after_tokens": afterTokens, "summary": summary, "usage": usage, "info": info, "messages": wire})
}

// Manual compression of a live session in process
func sessionCompress(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sess(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	sid := strOr(params["session_id"])
	var result map[string]any
	var raised any
	var turnErr error
	raised = catchPanic(func() {
		turnErr = manualCompressTurn(ctx, sid, session, func() error {
			result = compressLive(ctx, rid, sid, session, strParam(params, "focus_topic", ""))
			return nil
		})
	})
	if raised == nil && turnErr == nil {
		return result
	}
	failure := turnErr
	if failure == nil {
		if asErr, isErr := raised.(error); isErr {
			failure = asErr
		} else {
			failure = fmt.Errorf("%s", pyrepr.Str(raised))
		}
	}
	var busy *CompressionBusy
	var held *CompressionLockHeld
	switch {
	case errors.As(failure, &busy):
		return rpcErr(rid, 4009, busy.Error(), nil)
	case errors.As(failure, &held):
		statusUpdate(ctx, sid, "ready", nil)
		return ok(rid, map[string]any{"compressed": false, "lock_held": true, "message": agent.DescribeCompressionLockSkip(held.Holder)})
	}
	agent.FinalizeContextEngineCompressionNotification(sessionAgent(session), false)
	return rpcErr(rid, 5005, failure.Error(), nil)
}

// Saves the session as JSON under the session's profile home, from the stored row else the in memory history
func sessionSave(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
	var data *pyjson.Object
	var loadErr error
	if failure := catchPanic(func() {
		scoped := sessionProfileRuntimeScope(ctx, session, false)
		db, release := sessionDB(scoped, session)
		defer release()
		if db != nil {
			data, loadErr = state.LoadSaveSnapshot(scoped, db, submitRowTargetKey(session), "json")
		}
	}); failure != nil {
		if asErr, isErr := failure.(error); isErr {
			loadErr = asErr
		} else {
			loadErr = fmt.Errorf("%s", pyrepr.Str(failure))
		}
	}
	if loadErr != nil {
		var tooLarge *state.SessionExportTooLargeError
		if errors.As(loadErr, &tooLarge) {
			return rpcErr(rid, 4131, loadErr.Error(), nil)
		}
		pkgLog().Warn("session.save: stored-session read failed; saving the in-memory history", "error", loadErr)
		data = nil
	}
	profileHome := strOr(session.Get("profile_home"))
	if profileHome == "" {
		profileHome = launchHome()
	}
	path := filepath.Join(profileHome, "sessions", "saved", "icebath_conversation_"+pydatetime.Strftime(time.Now(), "%Y%m%d_%H%M%S")+".json")
	if data == nil || len(data.Vals) == 0 {
		a := sessionAgent(session)
		var started any = session.Get("created_at")
		model, systemPrompt := "", ""
		if a != nil {
			started = float64(a.SessionStart().UnixNano()) / 1e9
			model, systemPrompt = a.Model(), a.CachedSystemPrompt()
		}
		switch started.(type) {
		case int, int64, float64:
		default:
			started = nil
		}
		unlock := lockHistory(session)
		data = pyjson.NewObject()
		data.Set("id", submitRowTargetKey(session))
		data.Set("model", model)
		data.Set("started_at", started)
		data.Set("system_prompt", systemPrompt)
		data.Set("messages", append([]any{}, historyList(session.Get("history"))...))
		unlock()
	}
	rendered, err := state.RenderSessionForSave(data, "json")
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, []byte(rendered), 0o644)
		}
	}
	if err != nil {
		return rpcErr(rid, 5011, fmt.Sprintf("failed to save %s: %s", path, err), nil)
	}
	return ok(rid, map[string]any{"file": path})
}

// Pops the live session under the resume lock and finalizes it, returning its end messages
func sessionClose(ctx context.Context, rid any, params map[string]any) map[string]any {
	sessionResumeLock.Lock()
	session := popSessionByID(strOr(orEmpty(params["session_id"])))
	sessionResumeLock.Unlock()
	closed := teardownPoppedSession(ctx, session, "tui_close")
	messages := []any{}
	if session != nil {
		messages = append(messages, historyList(session.Get("_end_msgs"))...)
	}
	return ok(rid, map[string]any{"closed": closed, "messages": messages})
}

// Stores the client terminal width on the session
func terminalResize(ctx context.Context, rid any, params map[string]any, session *tuihost.Session) map[string]any {
	var raw any = 80
	if value, present := params["cols"]; present {
		raw = value
	}
	cols, err := pyint.Int64(raw)
	panicIf(err)
	session.Set("cols", int(cols))
	return ok(rid, map[string]any{"cols": int(cols)})
}
