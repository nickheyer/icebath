package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Refusal reasons when the owner FIFO could not take a typed prompt or did not confirm it in time
const (
	sessionOwnerBusy        = "SESSION_OWNER_BUSY"
	sessionOwnerUnconfirmed = "SESSION_OWNER_UNCONFIRMED"
)

// Exact field set of a hosted room turn proof
var hostedTaskFields = []string{"room_id", "task_id", "thread_id", "turn_id", "execution_generation", "member_id"}

var promptSubmitRegistry = &handlerRegistry{}

func init() {
	promptSubmitRegistry.method("prompt.submit", promptSubmit)
	promptSubmitRegistry.install()
}

// Reply ending voice chat for a typed bare stop phrase while voice mode is on, nil otherwise
func typedStopPhraseResponse(ctx context.Context, rid any, text any, sid string) map[string]any {
	stopped, err := consumeTypedStopPhrase(ctx, text, sid, true)
	panicIf(err)
	if stopped {
		return ok(rid, map[string]any{"voice_stopped": true})
	}
	return nil
}

// Hosted room terminal callback as the turn receipt hook, a failing commit raising out of the turn
func hostedTerminalHook(v any) (func(map[string]any), bool) {
	switch cb := v.(type) {
	case func(map[string]any) error:
		if cb != nil {
			return func(receipt map[string]any) { panicIf(cb(receipt)) }, true
		}
	case func(map[string]any):
		if cb != nil {
			return cb, true
		}
	}
	return nil, false
}

// Image paths of a hosted submit, false unless every entry is a non empty string
func hostedImagePaths(v any) ([]string, bool) {
	var items []any
	switch list := v.(type) {
	case []any:
		items = list
	case []string:
		for _, item := range list {
			items = append(items, item)
		}
	default:
		return nil, false
	}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		s, isStr := item.(string)
		if !isStr || s == "" {
			return nil, false
		}
		paths = append(paths, s)
	}
	return paths, true
}

// Validates the hosted room turn proof an internal submit carries, nil when it holds
func hostedSubmitError(rid any, session *tuigateway.Session, hostedTask, hostedCallback, hostedImages any) map[string]any {
	if session.Get("source") != "bot_room" {
		return rpcErr(rid, 4120, "hosted room turns require a bot_room session", nil)
	}
	task, isDict := hostedTask.(map[string]any)
	_, callable := hostedTerminalHook(hostedCallback)
	valid := isDict && callable
	if valid && hostedImages != nil {
		_, valid = hostedImagePaths(hostedImages)
	}
	if valid && len(task) != len(hostedTaskFields) {
		valid = false
	}
	for _, field := range hostedTaskFields {
		if !valid {
			break
		}
		value, present := task[field]
		if !present {
			valid = false
		} else if field == "execution_generation" {
			_, valid = pyint.Instance(value)
		} else {
			s, isStr := value.(string)
			valid = isStr && s != ""
		}
	}
	if valid {
		return nil
	}
	return rpcErr(rid, 4120, "invalid hosted room turn proof", nil)
}

// Fences direct prompts into a hosted room from older Desktop builds that only know the Group title
func legacyGroupFenceError(ctx context.Context, rid any, session *tuigateway.Session, params map[string]any) map[string]any {
	title := strOr(session.Get("title"))
	roomID := ""
	if strings.HasPrefix(title, "Group: ") {
		roomID = strings.TrimSpace(strings.TrimPrefix(title, "Group: "))
	}
	if roomID == "" {
		return nil
	}
	var hosted, peer bool
	var probeErr error
	if failure := catchPanic(func() {
		dbPath := gateway.HostedRoomsDefaultDBPath(ctx)
		hosted, probeErr = gateway.ProbeHostedRoom(ctx, dbPath, roomID)
		if probeErr != nil || hosted {
			return
		}
		target := home.ProfileNameFor(strOr(session.Get("profile_home")))
		if target == "" {
			target = strings.TrimSpace(strOr(params["profile"]))
		}
		if target == "" {
			target = currentProfileName(ctx)
			if target == "" {
				target = "default"
			}
			target = strings.TrimSpace(target)
		}
		peer, probeErr = gateway.ProbePeerRoomReservation(ctx, dbPath, roomID, target, nil)
	}); failure != nil {
		return rpcErr(rid, 5122, groupProbeFailedMsg, nil)
	}
	if probeErr != nil {
		var unavailable *gateway.RoomProbeUnavailableError
		var roomErr *gateway.HostedRoomError
		if errors.As(probeErr, &unavailable) {
			return rpcErr(rid, 5122, groupProbeFailedMsg, nil)
		}
		if errors.As(probeErr, &roomErr) {
			return nil
		}
		return rpcErr(rid, 5122, groupProbeFailedMsg, nil)
	}
	if hosted || peer {
		owner := "its home host"
		if hosted {
			owner = "its gateway"
		}
		return rpcErr(rid, 4122, fmt.Sprintf("This room is managed by %s. Update Hermes Desktop to continue it.", owner), nil)
	}
	return nil
}

// Machine readable storage error data whose code lets a GUI pick a doctor or retry action
func storageErrorData(failure state.StorageFailure, raw any) map[string]any {
	return map[string]any{"code": failure.Code, "cause": failure.Cause, "details": state.StorageFailureDetails(raw, 200)}
}

// Persists the session row and the submitted message, releasing the turn with a storage error reply on failure
func persistSessionRowForSubmit(ctx context.Context, rid any, session *tuigateway.Session, text any, displayKind string) map[string]any {
	var errResp map[string]any
	persisted := false
	failure := catchPanic(func() {
		if !ensureSessionDBRow(ctx, session) {
			dbMu.Lock()
			cause := dbError
			dbMu.Unlock()
			storage := state.DescribeStorageFailure(ctx, cause)
			errResp = rpcErr(rid, 5072, fmt.Sprintf("Session storage is unavailable, so this message was not saved. Cause: %s. "+
				"%s Then send your message again.", storage.Gloss, storage.Action), storageErrorData(storage, cause))
			return
		}
		persistBranchSeed(ctx, session)
		db, release := sessionDB(ctx, session)
		func() {
			defer release()
			if db != nil {
				panicIf(reopenIfFinalized(ctx, db, strOr(session.Get("session_key"))))
			}
		}()
		persistSubmitUserRow(ctx, session, text, displayKind, nil)
		persisted = true
	})
	if failure != nil {
		storage := state.DescribeStorageFailure(ctx, failure)
		if storage.Code == "disk_full" {
			errResp = rpcErr(rid, 5070, "Session storage could not be written, so this message was not saved: the disk is full. "+
				"Free some disk space, then send your message again.", storageErrorData(storage, failure))
		} else {
			logger().Warn("prompt.submit: session persist failed: "+pyrepr.Str(failure), "error", failure)
			errResp = rpcErr(rid, 5071, fmt.Sprintf("Session storage could not be written, so this message was not saved. Cause: %s. "+
				"%s Then send your message again.", storage.Gloss, storage.Action), storageErrorData(storage, failure))
		}
	} else if persisted {
		return nil
	}
	unlock := lockHistory(session)
	session.Set("running", false)
	session.Set("last_active", nowSeconds())
	session.Pop("_hosted_room_task")
	clearInflightTurn(session)
	releaseActiveSessionSlot(session)
	unlock()
	return errResp
}

// Turn thread body waiting patiently for a deferred build before running the accepted prompt
func runAfterAgentReady(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, displayKind string, displayMetadata map[string]any, terminalCallback func(map[string]any), turnAuthor map[string]any, hostedImages []string) {
	if errResp := waitAgentForPrompt(ctx, session, rid, sid); errResp != nil {
		message := "agent initialization failed"
		if detail, isDict := errResp["error"].(map[string]any); isDict {
			if text, present := detail["message"]; present {
				message = pyrepr.Str(text)
			}
		}
		emitTerminalTurnError(ctx, sid, session, message,
			map[string]any{"layer": "runtime", "code": "agent_init_failed", "retryable": true}, true)
		unlock := lockHistory(session)
		session.Set("running", false)
		session.Set("last_active", nowSeconds())
		unlock()
		tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, sessionAgent(session), session))
		return
	}
	unlock := lockHistory(session)
	if truthy(session.Get("_turn_cancel_requested")) || !truthy(session.Get("running")) {
		session.Set("running", false)
		clearInflightTurn(session)
		message := "Session no longer running before the agent was ready"
		if truthy(session.Get("_turn_cancel_requested")) {
			message = "Turn cancelled before the agent was ready"
		}
		unlock()
		tuigateway.Emit(ctx, "error", sid, map[string]any{"message": message})
		return
	}
	unlock()
	runPromptSubmit(ctx, rid, sid, session, text, promptSubmitOptions{displayKind: displayKind, displayMetadata: displayMetadata,
		imagePaths: hostedImages, terminalCallback: terminalCallback, turnAuthor: turnAuthor})
}

// Answer to a refused slot, a ledger refusal of a client submit going to the owner FIFO
func slotRefusal(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, params map[string]any, refusal *activesessions.Refusal, internalHostedSubmit bool) map[string]any {
	if refusal.LedgerStatus != "" && !internalHostedSubmit {
		return submitToLedgerOwner(ctx, rid, sid, session, text, params, refusal)
	}
	var data any
	if refusal.Reason != "" {
		data = map[string]any{"reason": refusal.Reason}
	}
	return rpcErr(rid, 4090, refusal.Message, data)
}

// Queues the prompt behind the gateway ledger work in the owner FIFO, else refuses with the reason it cannot join
func submitToLedgerOwner(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, params map[string]any, refusal *activesessions.Refusal) map[string]any {
	reason := refusal.Reason
	if reason == "" {
		reason = "SESSION_NOT_OWNED"
	}
	notOwned := rpcErr(rid, 4090, refusal.Message, map[string]any{"reason": reason})
	if refusal.LedgerStatus == "unknown" {
		return notOwned
	}
	why := ""
	if !ownerForwardable(refusal, session, text, params) {
		why = "this kind of message cannot join its queue"
		if s, isStr := text.(string); isStr && strings.TrimSpace(s) != "" {
			why = "a rewind or attachment cannot join its queue"
		}
		if unstageRefusedImages(session) > 0 {
			why = "an image cannot join its queue, so it was removed from this message"
		}
	} else {
		receipt, err := forwardPrompt(ctx, sid, session, text.(string), nil, "")
		var owner *OwnerUnavailableError
		switch {
		case err == nil:
			if receipt["voice_stopped"] == true {
				return ok(rid, map[string]any{"voice_stopped": true})
			}
			return ok(rid, map[string]any{"status": "queued", "admission_id": receipt["admission_id"], "forwarded": true})
		case errors.As(err, &owner) && owner.NotRunning:
			return notOwned
		case errors.As(err, &owner) && owner.Unconfirmed:
			return rpcErr(rid, 4090, "The gateway is running a turn in this chat and did not confirm your message"+
				" in time. It may already be queued there: check the chat before sending it again.",
				map[string]any{"reason": sessionOwnerUnconfirmed})
		default:
			key := strOr(session.Get("session_key"))
			if key == "" {
				key = sid
			}
			logger().Info(fmt.Sprintf("forwarding prompt for %s to the owner failed: %s", key, err), "error", err)
			why = "the gateway refused it"
			if errors.As(err, &owner) {
				why = err.Error()
			}
		}
	}
	return rpcErr(rid, 4090, "The gateway is running a turn in this chat and "+why+
		". Nothing was sent. Send it again when the current turn ends.", map[string]any{"reason": sessionOwnerBusy})
}

// Drops the images staged for a submit the owner FIFO cannot take, returning how many were dropped
func unstageRefusedImages(session *tuigateway.Session) int {
	unlock := lockHistory(session)
	defer unlock()
	dropped := len(historyList(session.Get("attached_images")))
	session.Set("attached_images", []any{})
	return dropped
}

// Under the history lock refuses watch child races and malformed truncation, applies the cut and marks the turn running
func lockInSubmitTurn(ctx context.Context, rid any, sid string, session *tuigateway.Session, text any, params map[string]any, hasTruncation bool, requestedRebindIDs map[int64]bool, hostedTask any, displayKind string) (errResp map[string]any, fields map[string]any) {
	fields = map[string]any{}
	sessionTurnAdmission(session, func(admitted bool) {
		if !admitted {
			errResp = rpcErr(rid, 5035, "backend is retiring; reconnect to continue", nil)
			return
		}
		if truthy(session.Get("lazy")) {
			var profileHome any
			if truthy(session.Get("profile_home")) {
				profileHome = session.Get("profile_home")
			}
			if childRunActive(strOr(session.Get("session_key")), profileHome) {
				errResp = rpcErr(rid, 4009, "subagent still running — wait for it to finish", nil)
				return
			}
		}
		if provider.IsTruthyValue(params["confirm_truncate"]) && !hasTruncation {
			errResp = rpcErr(rid, 4004,
				"confirm_truncate requires truncate_before_user_ordinal, truncate_before_message_id, or truncate_before_row_id", nil)
			return
		}
		if hasTruncation {
			var cutErr map[string]any
			cutErr, fields = truncateHistoryForSubmit(ctx, rid, sid, session, params, requestedRebindIDs)
			if cutErr != nil {
				errResp, fields = cutErr, map[string]any{}
				return
			}
		}
		session.Set("running", true)
		session.Set("_turn_cancel_requested", false)
		session.Set("last_active", nowSeconds())
		if hostedTask != nil {
			session.Set("_hosted_room_task", copyDict(hostedTask))
		}
		startInflightTurn(session, text, displayKind, nil)
	})
	return errResp, fields
}

// First rune prefix of a string, the str slice of Python
func runePrefix(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// Admits a prompt into the session, claiming the turn, applying a confirmed rewind and starting the turn thread
func promptSubmit(ctx context.Context, rid any, params map[string]any) map[string]any {
	sid := strOr(params["session_id"])
	var text any = ""
	if raw, present := params["text"]; present {
		text = raw
	}
	if s, isStr := text.(string); isStr {
		text = tools.SanitizeUserPromptText(s)
	}
	displayKind := ""
	if params["display_kind"] == "hidden" {
		displayKind = "hidden"
	}
	var displayMetadata map[string]any
	if preview, isStr := params["title_preview"].(string); isStr && strings.TrimSpace(preview) != "" {
		displayMetadata = map[string]any{"title_preview": runePrefix(preview, 1000)}
	}
	if stopped := typedStopPhraseResponse(ctx, rid, text, sid); stopped != nil {
		return stopped
	}
	if truthy(params["interrupted"]) {
		media.MarkSpeechInterrupted()
	}
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	var turnAuthor map[string]any
	if raw := params["_turn_author"]; raw != nil {
		author, isAuthor := raw.(*tools.DeliveryAuthor)
		if !isAuthor || author == nil {
			return rpcErr(rid, 4124, "turn author is stamped by the gateway, never by a client", nil)
		}
		turnAuthor = author.Author
	}
	hostedTask, hostedCallback, hostedImages := params["_hosted_task"], params["_hosted_terminal_callback"], params["_hosted_images"]
	internalHostedSubmit := hostedTask != nil || hostedCallback != nil || hostedImages != nil
	if internalHostedSubmit {
		errResp = hostedSubmitError(rid, session, hostedTask, hostedCallback, hostedImages)
	} else {
		errResp = legacyGroupFenceError(ctx, rid, session, params)
	}
	if errResp != nil {
		return errResp
	}
	if refusal := ensureActiveSessionSlot(ctx, sid, session); refusal != nil {
		return slotRefusal(ctx, rid, sid, session, text, params, refusal, internalHostedSubmit)
	}
	surface, _ := params["surface"].(string)
	if !gateway.IsTurnSurface(surface) {
		surface = ""
	}
	session.Set("client_surface", surface)
	voiceContext, isStr := params["voice_context"].(string)
	if surface != "voice-live" || !isStr {
		voiceContext = ""
	}
	session.Set("voice_live_context", runePrefix(voiceContext, 6000))
	session.Set("voice_turn", params["voice_turn"] == true)
	hasTruncation := false
	for _, key := range truncationParams {
		if params[key] != nil {
			hasTruncation = true
		}
	}
	if s, isStr := text.(string); hasTruncation && isStr {
		text = tuigateway.ExpandSkillInvocationForReplay(ctx, s, strOr(session.Get("session_key")))
	}
	turnIsolation := sessionUsesComputeHost(ctx, session, nil)
	if internalHostedSubmit && turnIsolation {
		return rpcErr(rid, 4121, "hosted room turns do not support isolated compute workers yet", nil)
	}
	sessionResumeLock.Lock()
	if refusal := reattachRefusal(rid, sid, session); refusal != nil {
		sessionResumeLock.Unlock()
		return refusal
	}
	t := tuigateway.CurrentTransport(ctx)
	if t != nil {
		rebindLiveTransport(ctx, sid, session, t)
	}
	sessionResumeLock.Unlock()
	for {
		unlock := lockHistory(session)
		if !truthy(session.Get("running")) {
			unlock()
			break
		}
		if internalHostedSubmit {
			unlock()
			return rpcErr(rid, 4091, "hosted room member session is busy", nil)
		}
		var busyTransport any = t
		if t == nil {
			busyTransport = session.Get("transport")
		}
		unlock()
		if hasTruncation {
			return rpcErr(rid, 4009, "session busy", nil)
		}
		if busy := handleBusySubmit(ctx, rid, sid, session, text, busyTransport, truthy(params["queued"]), turnAuthor, displayKind); busy != nil {
			return busy
		}
	}
	var requestedRebindIDs map[int64]bool
	if rawIDs, isList := params["rebind_survivor_row_ids"].([]any); isList {
		requestedRebindIDs = map[int64]bool{}
		for _, raw := range rawIDs {
			if _, isBool := raw.(bool); isBool {
				continue
			}
			if n, isInt := pyrepr.Int(raw); isInt && n.IsInt64() {
				requestedRebindIDs[n.Int64()] = true
			}
		}
	}
	errResp, survivorFields := lockInSubmitTurn(ctx, rid, sid, session, text, params, hasTruncation, requestedRebindIDs, hostedTask, displayKind)
	if errResp != nil {
		return errResp
	}
	if turnIsolation {
		if turnAuthor != nil {
			logger().Debug(fmt.Sprintf("isolated compute turns carry no author yet; the turn from %s runs unattributed", pyrepr.Str(turnAuthor["id"])))
		}
		if failure := catchPanic(func() {
			db, release := sessionDB(ctx, session)
			defer release()
			if db != nil {
				panicIf(reopenIfFinalized(ctx, db, strOr(session.Get("session_key"))))
			}
		}); failure != nil {
			logger().Debug("finalized-session reopen before isolated dispatch failed for "+sid, "error", failure)
		}
		isolated := submitPromptToComputeHost(ctx, rid, sid, session, text, nil, nil, displayKind, displayMetadata)
		if isolated["error"] == nil {
			if result, isDict := isolated["result"].(map[string]any); isDict {
				for key, value := range survivorFields {
					result[key] = value
				}
			}
			return isolated
		}
		message := "unknown error"
		if detail, isDict := isolated["error"].(map[string]any); isDict {
			if m, present := detail["message"]; present {
				message = pyrepr.Str(m)
			}
		}
		logger().Warn(fmt.Sprintf("compute-host dispatch failed for session %s; falling back inline: %s", sid, message))
	}
	if errResp := persistSessionRowForSubmit(ctx, rid, session, text, displayKind); errResp != nil {
		return errResp
	}
	if staged, isDict := session.Get("_submit_user_row").(map[string]any); isDict {
		if rowID, isInt := pyint.Instance(staged["_row_id"]); isInt {
			survivorFields["user_row_id"] = pyint.Value(rowID)
		}
	}
	failedReady, _ := session.Get("agent_ready").(*pythread.Event)
	if !restartCompletedFailedAgentBuild(ctx, sid, session, failedReady) {
		startAgentBuild(ctx, sid, session)
	}
	terminalCallback, _ := hostedTerminalHook(hostedCallback)
	var imagePaths []string
	if hostedImages != nil {
		imagePaths, _ = hostedImagePaths(hostedImages)
	}
	runThread := memory.SpawnContextThread(ctx, "", func(threadCtx context.Context) {
		runAfterAgentReady(threadCtx, rid, sid, session, text, displayKind, displayMetadata, terminalCallback, turnAuthor, imagePaths)
	})
	session.Set("_run_thread", runThread)
	runThread.Start()
	result := map[string]any{"status": "streaming"}
	for key, value := range survivorFields {
		result[key] = value
	}
	return ok(rid, result)
}
