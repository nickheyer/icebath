package tuiserver

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Schedules a continuation turn for a crash interrupted session, its resume descriptor or nil
func maybeScheduleAutoContinue(ctx context.Context, sid string, session *tuigateway.Session, sessionKey string) map[string]any {
	if session.Get("source") == "bot_room" {
		return nil
	}
	h := sessionHome(ctx, session)
	marker := tuigateway.ReadTurnMarker(h, sessionKey)
	if marker == nil || !truthy(marker["auto_continue"]) {
		return nil
	}
	writerState := tuigateway.MarkerWriterState(marker)
	if writerState == "alive" && !pyjsonEqual(marker["writer_pid"], int64(os.Getpid())) {
		logger().Info(fmt.Sprintf("auto-continue for %s held back: marker writer pid %s is still alive (reader pid %d); the marker is ownership evidence and the owner clears it",
			sessionKey, pyrepr.Str(marker["writer_pid"]), os.Getpid()))
		return nil
	}
	enabled, freshnessSecs, maxAttempts := autoContinueConfig(ctx)
	startedAt := marker["started_at"].(float64)
	attempts := marker["attempts"].(int64)
	age := nowSeconds() - startedAt
	if !enabled || age > freshnessSecs || attempts >= int64(maxAttempts) {
		tuigateway.ClearTurnMarker(h, sessionKey)
		return nil
	}
	if truthy(session.Get("_auto_continue_scheduled")) {
		return nil
	}
	session.Set("_auto_continue_scheduled", true)
	attempt, text := attempts+1, autoContinueNote(pyrepr.Str(marker["prompt"]))
	kickoff := func(ctx context.Context) {
		rid := fmt.Sprintf("__auto_continue__%d", time.Now().UnixMilli())
		var errResp map[string]any
		if failure := catchPanic(func() {
			startAgentBuild(ctx, sid, session)
			errResp = waitAgent(ctx, session, rid, 120.0)
		}); failure != nil {
			logger().Warn("auto-continue agent build failed for "+sid, "error", pyrepr.Str(failure))
			errResp = map[string]any{"error": map[string]any{"message": "agent build failed"}}
		}
		if len(errResp) > 0 {
			session.Set("_auto_continue_scheduled", false)
			return
		}
		unlock := lockHistory(session)
		if truthy(session.Get("running")) || truthy(session.Get("_turn_cancel_requested")) || truthy(session.Get("_finalized")) {
			session.Set("_auto_continue_scheduled", false)
			unlock()
			return
		}
		session.Set("running", true)
		session.Set("last_active", nowSeconds())
		unlock()
		if ensureActiveSessionSlot(ctx, sid, session) != nil {
			logger().Info("auto-continue for " + sessionKey + " refused: session has another live owner")
			unlock = lockHistory(session)
			session.Set("running", false)
			session.Set("_auto_continue_scheduled", false)
			unlock()
			return
		}
		unlock = lockHistory(session)
		session.Set("_auto_continue_attempt", attempt)
		session.Set("_auto_continue_prompt", marker["prompt"])
		unlock()
		if failure := catchPanic(func() {
			diagnostic := marker["notification_category"] == "diagnostic"
			scoped := sessionProfileRuntimeScope(ctx, session, true)
			if _, err := config.RenderNotification(scoped, func() error {
				tuigateway.Emit(scoped, "status.update", sid, map[string]any{"kind": "process", "text": "Resuming interrupted turn…"})
				tuigateway.Emit(scoped, "message.start", sid, nil)
				return nil
			}, "tui", diagnostic, nil); err != nil {
				panic(err)
			}
			o := promptSubmitOptions{displayKind: "auto_continue"}
			if diagnostic {
				o.displayMetadata = map[string]any{"notification_category": "diagnostic"}
			}
			runPromptSubmit(scoped, rid, sid, session, text, o)
		}); failure != nil {
			notifLogFailure("auto-continue dispatch failed", failure)
			notifReleaseTurn(session)
		}
	}
	if startSessionWork(ctx, kickoff, "auto-continue-"+sid, nil) == nil {
		session.Set("_auto_continue_scheduled", false)
		return nil
	}
	logger().Info(fmt.Sprintf("auto-continue scheduled for session %s (attempt %d, interrupted %.0fs ago, writer pid %s: %s, reader pid %d)",
		sessionKey, attempt, age, pyrepr.Str(marker["writer_pid"]), writerState, os.Getpid()))
	return map[string]any{"attempt": attempt, "interrupted_at": startedAt}
}

// Interrupts a busy turn on its own goroutine, never under the history lock, at most one per session
func interruptBusySession(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent) {
	useAgent := a != nil
	if !useAgent && !sessionUsesComputeHost(ctx, session, loadCfg(ctx)) {
		return
	}
	unlock := lockHistory(session)
	if truthy(session.Get("_busy_interrupt_pending")) {
		unlock()
		return
	}
	session.Set("_busy_interrupt_pending", true)
	unlock()
	go func() {
		defer threadExceptHook("busy-interrupt-" + sid)
		defer func() {
			unlock := lockHistory(session)
			session.Set("_busy_interrupt_pending", false)
			unlock()
		}()
		suppress(func() {
			if useAgent {
				a.Interrupt("")
				return
			}
			supervisor, err := getComputeHostSupervisor(ctx, loadCfg(ctx))
			panicIf(err)
			panicIf(supervisor.Interrupt(sid, ""))
		})
	}()
}

// Makes a queued prompt durable at accept, the row staged on its envelope and merged text synced in place
func persistQueuedUserRow(ctx context.Context, session *tuigateway.Session, envelope map[string]any, displayKind string) {
	if sessionUsesComputeHost(ctx, session, loadCfg(ctx)) {
		return
	}
	ensureSessionDBRow(ctx, session)
	staged, _ := envelope["_submit_user_row"].(map[string]any)
	if rowID, ok := staged["_row_id"].(int64); ok {
		if pyjsonEqual(staged["content"], envelope["text"]) {
			return
		}
		var liveID int64
		updated := false
		db, release := sessionDB(ctx, session)
		if db == nil {
			release()
			return
		}
		if failure := catchPanic(func() {
			key := submitRowOwnerKey(staged, session)
			id, found, err := db.ResolveActiveRowID(ctx, key, rowID)
			panicIf(err)
			if found {
				n, err := db.SetUserMessageContent(ctx, key, id, envelope["text"], nil)
				panicIf(err)
				liveID, updated = id, n != 0
			}
		}); failure != nil {
			release()
			logger().Debug("queued-prompt row merge update failed", "error", pyrepr.Str(failure))
			return
		}
		release()
		if !updated {
			delete(envelope, "_submit_user_row")
			return
		}
		staged["_row_id"], staged["content"] = liveID, envelope["text"]
		return
	}
	written := writeSubmitUserRow(ctx, session, envelope["text"], displayKind, map[string]any{state.QueuedPromptMetadataKey: true})
	if written != nil {
		envelope["_submit_user_row"] = written
		if displayKind != "" {
			envelope["_queued_display_kind"] = displayKind
		}
	}
}

// Re places a queued prompt's accept time row at the transcript end before its turn, deactivating the early row
func replaceQueuedUserRowForTurn(ctx context.Context, session *tuigateway.Session, queued map[string]any, isDispatching bool) map[string]any {
	early, _ := queued["_submit_user_row"].(map[string]any)
	earlyID, ok := early["_row_id"].(int64)
	if !ok {
		return nil
	}
	displayKind, _ := queued["_queued_display_kind"].(string)
	if isDispatching {
		persistSubmitUserRow(ctx, session, queued["text"], displayKind, nil)
	} else {
		persistSubmitUserRow(ctx, session, queued["text"], displayKind, map[string]any{state.QueuedPromptMetadataKey: true})
	}
	fresh, _ := session.Get("_submit_user_row").(map[string]any)
	if _, freshOK := fresh["_row_id"].(int64); !freshOK {
		return nil
	}
	queued["_submit_user_row"] = fresh
	db, release := sessionDB(ctx, session)
	defer release()
	if db == nil {
		return nil
	}
	if failure := catchPanic(func() {
		key := submitRowOwnerKey(early, session)
		liveID, found, err := db.ResolveActiveRowID(ctx, key, earlyID)
		panicIf(err)
		if found {
			_, err = db.DeactivateMessage(ctx, key, liveID)
			panicIf(err)
		}
	}); failure != nil {
		logger().Debug("queued-prompt row re-placement deactivate failed", "error", pyrepr.Str(failure))
	}
	return fresh
}

// Applies display.busy_input_mode to a mid turn prompt, nil when the turn ended and the caller retries idle
func handleBusySubmit(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, transport any, queued bool, turnAuthor map[string]any, displayKind string) map[string]any {
	mode := "queue"
	if !queued {
		mode = loadBusyInputMode(ctx)
	}
	a := sessionAgent(session)
	if (mode == "steer" || mode == "interrupt") && sessionCompressionInFlight(ctx, session) {
		mode = "queue"
	}
	var imagePaths []string
	unlock := lockHistory(session)
	if !truthy(session.Get("running")) {
		unlock()
		return nil
	}
	session.Set("_turn_user_input", true)
	imagePaths = stringList(session.Get("attached_images"))
	if len(imagePaths) > 0 {
		session.Set("attached_images", []string{})
	}
	unlock()
	plainText := ""
	if len(imagePaths) == 0 && tuigateway.IsTextOnlyBusyPayload(text) {
		plainText = pystr.Strip(tuigateway.CoerceMessageText(text, true))
	}
	if plainText != "" && a != nil {
		method, status := "", ""
		supported := false
		switch mode {
		case "steer":
			method, status, supported = "steer", "steered", true
		case "interrupt":
			method, status, supported = "redirect", "redirected", a.SupportsActiveTurnRedirect()
		}
		if method != "" && supported {
			if resp := acTryCorrection(rid, session, a, method, plainText, status); resp != nil {
				return resp
			}
		}
	}
	unlock = lockHistory(session)
	if !truthy(session.Get("running")) {
		if len(imagePaths) > 0 {
			session.Set("attached_images", append(imagePaths, stringList(session.Get("attached_images"))...))
		}
		unlock()
		return nil
	}
	if envelope := enqueuePrompt(ctx, session, text, transport, imagePaths, turnAuthor); envelope != nil {
		persistQueuedUserRow(ctx, session, envelope, displayKind)
	}
	session.Set("last_active", nowSeconds())
	unlock()
	if mode == "interrupt" && len(imagePaths) == 0 {
		interruptBusySession(ctx, sid, session, a)
	}
	return ok(rid, map[string]any{"status": "queued"})
}

// Fires a queued next turn prompt when the session is idle, true when dispatched so lower priority follow ups wait
func drainQueuedPrompt(ctx context.Context, rid any, sid string, session *tuigateway.Session) bool {
	var queued map[string]any
	var generation int64
	claimed := false
	sessionTurnAdmission(session, func(admitted bool) {
		q, _ := session.Get("queued_prompt").(map[string]any)
		if !admitted || truthy(session.Get("_closing")) || len(q) == 0 || truthy(session.Get("running")) {
			return
		}
		queued, claimed = q, true
		generation = intOr(session.Get("_queued_prompt_generation"))
		rest, _ := session.Get("queued_prompts").([]any)
		acSetQueue(session, rest)
		session.Set("running", true)
		if t := transportOf(queued["transport"]); t != nil && !tuigateway.TransportIsDead(t) {
			tuigateway.AttachSessionTransport(session, t)
		}
	})
	if !claimed {
		return false
	}
	useComputeHost := sessionUsesComputeHost(ctx, session, loadCfg(ctx))
	unlock := lockHistory(session)
	if intOr(session.Get("_queued_prompt_generation")) != generation {
		entries := []any{queued}
		if advanced := session.Get("queued_prompt"); truthy(advanced) {
			entries = append(entries, advanced)
		}
		rest, _ := session.Get("queued_prompts").([]any)
		acSetQueue(session, append(entries, rest...))
		session.Set("running", false)
		unlock()
		return true
	}
	unlock()
	o := promptSubmitOptions{queuedPromptGeneration: &generation}
	if images := stringList(queued["image_paths"]); len(images) > 0 {
		o.imagePaths = images
	}
	unlock = lockHistory(session)
	dispatchRow := replaceQueuedUserRowForTurn(ctx, session, queued, true)
	var stillQueued []any
	if head := session.Get("queued_prompt"); truthy(head) {
		stillQueued = append(stillQueued, head)
	}
	rest, _ := session.Get("queued_prompts").([]any)
	stillQueued = append(stillQueued, rest...)
	for _, envelope := range stillQueued {
		if e, isMap := envelope.(map[string]any); isMap {
			replaceQueuedUserRowForTurn(ctx, session, e, false)
		}
	}
	if dispatchRow != nil {
		session.Set("_submit_user_row", dispatchRow)
	} else {
		session.Pop("_submit_user_row")
	}
	unlock()
	if author, isMap := queued["turn_author"].(map[string]any); isMap && len(author) > 0 {
		o.turnAuthor = author
	}
	dispatchFailed := false
	if failure := catchPanic(func() {
		if !useComputeHost {
			runPromptSubmit(ctx, rid, sid, session, queued["text"], o)
			return
		}
		resp := submitPromptToComputeHost(ctx, rid, sid, session, queued["text"], o.queuedPromptGeneration, o.imagePaths, "", nil)
		if errObj, failed := resp["error"]; failed && truthy(errObj) {
			unlock := lockHistory(session)
			session.Set("running", false)
			clearInflightTurn(session)
			unlock()
			message := "queued prompt failed"
			if e, isMap := errObj.(map[string]any); isMap && truthy(e["message"]) {
				message = pyrepr.Str(e["message"])
			}
			tuigateway.Emit(ctx, "error", sid, map[string]any{"message": message})
			dispatchFailed = true
		}
	}); failure != nil {
		notifLogFailure("queued prompt dispatch failed", failure)
		notifReleaseTurn(session)
		dispatchFailed = true
	}
	if dispatchFailed {
		unlock := lockHistory(session)
		drainNext := truthy(session.Get("queued_prompt")) && !truthy(session.Get("_turn_cancel_requested"))
		unlock()
		if drainNext {
			drainQueuedPrompt(ctx, rid, sid, session)
		}
	}
	return true
}
