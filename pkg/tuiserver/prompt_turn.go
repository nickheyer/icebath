package tuiserver

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session slot of the bounded goal retry after compression exhaustion
const goalCompressionRecoveryAttempts = "_goal_compression_recovery"

// Continuations a goal gets after compression exhaustion before it pauses
const goalCompressionRecoveryLimit = 1

// Bot Mode delivery text of a turn, a successful bare silence marker becoming invisible
func botModeDeliveryText(response any, successful bool) any {
	if !successful {
		return response
	}
	text, ok := response.(string)
	if !ok {
		return response
	}
	if gateway.IsIntentionalSilenceResponse(text) {
		return ""
	}
	return gateway.StripTrailingSilenceMarker(text)
}

// Whether a completion belongs to the canonical Bot Chat surface by title hint then live title
func isBotModeSession(ctx context.Context, session *tuigateway.Session) bool {
	if a := sessionAgent(session); a != nil {
		if hint := pystr.Strip(a.SessionTitleHint()); hint != "" {
			return hint == botmode.BotChatTitle
		}
	}
	return sessionLiveTitle(ctx, session, sessionLookupKey(session, "")) == botmode.BotChatTitle
}

// Reports a failed post turn hook on stderr with its exception type
func hookFailure(what string, err any) {
	fmt.Fprintf(os.Stderr, "[tui_gateway] %s failed: %s: %s\n", what, pyrepr.TypeName(err), pyrepr.Str(err))
}

// Whether a turn produced a real response the goal judge can use
func isSuccessfulGoalTurn(result map[string]any, status string, raw any) bool {
	text, ok := raw.(string)
	if status != "complete" || !ok || pystr.Strip(text) == "" {
		return false
	}
	if result == nil {
		return true
	}
	if pyrepr.Truthy(result["failed"]) || pyrepr.Truthy(result["interrupted"]) {
		return false
	}
	return result["completed"] != false || agent.IsMaxIterationHandoff(result)
}

// Goal manager of the session when a goal is active, else nil
func activeGoalManager(ctx context.Context, session *tuigateway.Session) *goals.GoalManager {
	maxTurns := int64(20)
	if goalsCfg, ok := loadCfg(ctx)["goals"].(map[string]any); ok {
		if raw, present := goalsCfg["max_turns"]; present && pyrepr.Truthy(raw) {
			if n, err := pyint.Int64(raw); err == nil && n != 0 {
				maxTurns = n
			}
		}
	}
	mgr := goals.NewGoalManager(ctx, strOr(session.Get("session_key")), maxTurns)
	if mgr.IsActive() {
		return mgr
	}
	return nil
}

// Bounded active goal retry after compression exhaustion as its continuation and notice
func planGoalCompressionRecovery(ctx context.Context, session *tuigateway.Session, result map[string]any, status string, raw any) (string, string) {
	if result == nil || !pyrepr.Truthy(result["compression_exhausted"]) {
		if isSuccessfulGoalTurn(result, status, raw) {
			session.Pop(goalCompressionRecoveryAttempts)
		}
		return "", ""
	}
	if strOr(session.Get("session_key")) == "" {
		return "", ""
	}
	mgr := activeGoalManager(ctx, session)
	if mgr == nil {
		session.Pop(goalCompressionRecoveryAttempts)
		return "", ""
	}
	state := mgr.State()
	goalCreatedAt, goalText := state.CreatedAt, state.Goal
	if goalText == nil {
		goalText = ""
	}
	attempts := int64(0)
	if recovery, ok := session.Get(goalCompressionRecoveryAttempts).(map[string]any); ok &&
		recovery["goal_created_at"] == goalCreatedAt && pyjsonEqual(recovery["goal"], goalText) {
		if raw := recovery["attempts"]; pyrepr.Truthy(raw) {
			if n, err := pyint.Int64(raw); err == nil {
				attempts = n
			}
		}
	}
	continuation := mgr.NextContinuationPrompt()
	if attempts < goalCompressionRecoveryLimit && continuation != nil && *continuation != "" {
		session.Set(goalCompressionRecoveryAttempts, map[string]any{"goal_created_at": goalCreatedAt, "goal": goalText, "attempts": attempts + 1})
		return *continuation, "Context compression was exhausted. Retrying the active goal once."
	}
	mgr.Pause("context compression exhausted twice consecutively")
	session.Pop(goalCompressionRecoveryAttempts)
	return "", "Goal paused after context compression was exhausted twice. Run /compress, then /goal resume to continue."
}

// Ownership and liveness gate every turn source crosses, the turn images and agent when admitted
func admitPromptTurn(ctx context.Context, sid string, session *tuigateway.Session, text any, imagePaths []string, queuedPromptGeneration *int64, displayKind string, displayMetadata map[string]any, reportOwnershipRefusal bool) ([]string, *agent.AIAgent, bool) {
	heldLease := session.Get("active_session_lease")
	if !truthy(session.Get("_closing")) {
		if refusal := ensureActiveSessionSlot(ctx, sid, session); refusal != nil {
			reason := refusal.Reason
			if reason == "" {
				reason = "refused"
			}
			logger().Info(fmt.Sprintf("Refusing turn for session %s at _run_prompt_submit: %s", sessionKeyOr(session, sid), reason))
			unlock := lockHistory(session)
			session.Set("running", false)
			session.Pop("_submit_user_row")
			unlock()
			if reportOwnershipRefusal {
				tuigateway.Emit(ctx, "error", sid, map[string]any{"message": refusal.Message})
			}
			return nil, nil, false
		}
	}
	var images []string
	var a *agent.AIAgent
	refused := func() bool {
		unlock := lockHistory(session)
		defer unlock()
		if truthy(session.Get("_closing")) || (queuedPromptGeneration != nil && intOr(session.Get("_queued_prompt_generation")) != *queuedPromptGeneration) {
			session.Set("running", false)
			session.Pop("_submit_user_row")
			if truthy(session.Get("_closing")) && session.Get("active_session_lease") != heldLease {
				releaseActiveSessionSlot(session)
			}
			return true
		}
		if imagePaths == nil {
			images = stringList(session.Get("attached_images"))
			session.Set("attached_images", []string{})
		} else {
			images = append([]string{}, imagePaths...)
		}
		inflight, ok := session.Get("inflight_turn").(map[string]any)
		if !ok || inflight["status"] == "error" {
			startInflightTurn(session, text, displayKind, displayMetadata)
		}
		a = sessionAgent(session)
		if a == nil {
			session.Set("running", false)
		} else {
			suppress(a.ClearInterrupt)
		}
		return false
	}()
	if refused {
		return nil, nil, false
	}
	if a == nil {
		reason := session.Get("agent_error")
		if !truthy(reason) {
			reason = agentMissingForTurn
		}
		logger().Info(fmt.Sprintf("Refusing turn for session %s: no agent attached (%s)", sessionKeyOr(session, sid), pyrepr.Str(reason)))
		emitTerminalTurnError(ctx, sid, session, reason, map[string]any{"layer": "runtime", "code": "agent_init_failed", "retryable": true}, true)
		return nil, nil, false
	}
	return images, a, true
}

// Writes the durable crash marker and returns the session key it was written under
func recordTurnMarker(ctx context.Context, session *tuigateway.Session, text any, autoContinue bool, notificationCategory string) string {
	markerHome := sessionHome(ctx, session)
	markerKey := strOr(session.Get("session_key"))
	attemptRaw, _ := session.Pop("_auto_continue_attempt")
	markerAttempt := int(intOr(orZero(attemptRaw)))
	markerText := text
	if prompt, _ := session.Pop("_auto_continue_prompt"); truthy(prompt) {
		markerText = prompt
	}
	if s, ok := markerText.(string); ok && pystr.Strip(s) != "" {
		unlock := lockHistory(session)
		session.Set("_active_turn_marker_key", markerKey)
		unlock()
		category := ""
		if notificationCategory == "diagnostic" {
			category = "diagnostic"
		}
		tuigateway.RecordTurnStart(markerHome, markerKey, s, markerAttempt, autoContinue, category)
		unlock = lockHistory(session)
		cancelled := truthy(session.Get("_turn_cancel_requested"))
		unlock()
		if cancelled {
			tuigateway.ClearTurnMarker(markerHome, markerKey)
		}
	}
	return markerKey
}

// Run message of a turn with attached images as native content parts or text path references
func routeTurnImages(ctx context.Context, a *agent.AIAgent, prompt any, images []string) any {
	promptText, _ := prompt.(string)
	mode := func() (mode string) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[tui_gateway] image_routing decision failed, defaulting to text: %s\n", pyrepr.Str(r))
				mode = "text"
			}
		}()
		providerName, model := activeImageRoutingIdentity(ctx, a)
		mode = tools.DecideImageInputMode(ctx, configEnv(ctx), providerName, model, a.RequestedProvider())
		if a.APIMode() == "codex_app_server" {
			mode = "text"
		}
		return mode
	}()
	if mode != "native" {
		return tuigateway.BuildImageRefMessage(promptText, images)
	}
	parts, skipped, err := tools.BuildNativeContentParts(configEnv(ctx), promptText, images, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tui_gateway] native attach failed, falling back to text: %s\n", err)
		return tuigateway.BuildImageRefMessage(promptText, images)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "[tui_gateway] native image attachment skipped %d unreadable path(s)\n", len(skipped))
	}
	for _, part := range parts {
		if p, ok := part.(map[string]any); ok && p["type"] == "image_url" {
			return parts
		}
	}
	return tuigateway.BuildImageRefMessage(promptText, images)
}

// Arms voice mode turn audio, the streaming queue first so a cut previous reply latches the barge in
func startTurnVoice(ctx context.Context) (*ttsTurnQueue, bool) {
	queue := ttsStreamBegin(ctx)
	if !voiceModeEnabled() {
		return queue, false
	}
	if bargeIn, present := voiceCfgDict(ctx)["barge_in"]; !present || truthy(bargeIn) {
		armFullDuplexListener(ctx)
	}
	started := false
	func() {
		defer func() {
			if recover() != nil {
				started = false
			}
		}()
		started = media.StartThinkingSound(ctx, func() bool {
			if media.IsAudioOutputActive() {
				return false
			}
			return !voiceContinuousActive()
		})
	}()
	return queue, started
}

// Writes the agent messages back to session history, a client warning when history desynced mid turn
func commitTurnHistory(session *tuigateway.Session, messages []map[string]any, history []any, historyVersion int64) string {
	unlock := lockHistory(session)
	defer unlock()
	currentVersion := intOr(session.Get("history_version"))
	if currentVersion == historyVersion {
		session.Set("history", messagesAsHistory(messages))
		session.Set("history_version", historyVersion+1)
		return ""
	}
	current := historyList(session.Get("history"))
	var historyNoMarkers, currentNoMarkers []any
	for _, e := range history {
		if !isPivotMarker(e) {
			historyNoMarkers = append(historyNoMarkers, e)
		}
	}
	anyMarker := false
	for _, e := range current {
		if isPivotMarker(e) {
			anyMarker = true
		} else {
			currentNoMarkers = append(currentNoMarkers, e)
		}
	}
	if pyjson.Equal(currentNoMarkers, historyNoMarkers) && anyMarker {
		newMessages := messagesAsHistory(messages)
		if len(messages) > len(history) {
			newMessages = newMessages[len(history):]
		}
		session.Set("history", append(append([]any{}, current...), newMessages...))
		session.Set("history_version", currentVersion+1)
		return ""
	}
	fmt.Fprintf(os.Stderr, "[tui_gateway] prompt.submit: history_version mismatch (expected=%d current=%d) — agent output NOT written to session history\n", historyVersion, currentVersion)
	return "History changed during this turn — the response above is visible but was not saved to session history."
}

// Interrupted, error or complete for a conversation result
func resultStatus(result map[string]any) string {
	if truthy(result["interrupted"]) {
		return "interrupted"
	}
	if truthy(result["error"]) {
		return "error"
	}
	return "complete"
}

// Raw text, status and last reasoning of a conversation result
func turnOutcome(result map[string]any, errorSurface map[string]any) (any, string, string) {
	raw, present := result["final_response"]
	if !present {
		raw = ""
	}
	status := resultStatus(result)
	if !truthy(raw) && truthy(result["error"]) && (truthy(result["failed"]) || truthy(result["partial"])) {
		raw = turnErrorText(result["error"], errorSurface, true)
	}
	if text, ok := raw.(string); ok && status == "interrupted" && strings.HasPrefix(pystr.Strip(text), agent.InterruptWaitingForModelPrefix) {
		raw = ""
	}
	lastReasoning := ""
	if lr, ok := result["last_reasoning"].(string); ok {
		lastReasoning = pystr.Strip(lr)
	}
	return raw, status, lastReasoning
}

// Goal continuation prompt to chain once running is released, empty when none
func goalFollowupAfterTurn(ctx context.Context, sid string, session *tuigateway.Session, result map[string]any, status string, raw any) string {
	followup := ""
	compressionExhausted := result != nil && truthy(result["compression_exhausted"])
	if err := catchPanic(func() {
		recoveryPrompt, recoveryNotice := planGoalCompressionRecovery(ctx, session, result, status, raw)
		if recoveryNotice != "" {
			var userConfig map[string]any
			if a := sessionAgent(session); a != nil {
				userConfig = a.NotificationConfig()
			}
			if _, err := config.RenderNotification(ctx, func() error {
				tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "goal", "text": recoveryNotice})
				return nil
			}, "tui", false, userConfig); err != nil {
				panic(err)
			}
		}
		followup = recoveryPrompt
	}); err != nil {
		hookFailure("goal compression recovery", err)
	}
	if compressionExhausted || !isSuccessfulGoalTurn(result, status, raw) {
		return followup
	}
	if err := catchPanic(func() {
		if !truthy(session.Get("session_key")) {
			return
		}
		mgr := activeGoalManager(ctx, session)
		if mgr == nil {
			return
		}
		activeDelegations := 0
		var background []map[string]any
		if err := catchPanic(func() {
			background = goals.GatherBackgroundProcesses(ctx, "", strOr(session.Get("session_key")))
			sessionID := ""
			if a := sessionAgent(session); a != nil {
				sessionID = a.SessionID()
			}
			activeDelegations = goals.CountActiveDelegations(sessionID)
		}); err != nil {
			background = nil
		}
		text, _ := raw.(string)
		decision, err := mgr.EvaluateAfterTurn(text, background, activeDelegations)
		if err != nil {
			panic(err)
		}
		if decision.Message != "" {
			tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "goal", "text": decision.Message})
		}
		if decision.ShouldContinue && decision.ContinuationPrompt != nil && *decision.ContinuationPrompt != "" {
			followup = *decision.ContinuationPrompt
		}
	}); err != nil {
		hookFailure("goal continuation hook", err)
	}
	return followup
}

// Hooks of a complete turn, the loop tick, the pending title and the voice fallback
func afterCompleteTurn(ctx context.Context, sid string, session *tuigateway.Session, st *turnRun, raw any) {
	if err := catchPanic(func() {
		key := strOr(session.Get("session_key"))
		if key == "" {
			return
		}
		mgr := agent.NewLoopManager(ctx, key)
		if state := mgr.State(); state != nil && state.AwaitingResponse {
			text, _ := raw.(string)
			if decision := mgr.CompleteTick(ctx, text); decision.Message != "" {
				tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "loop", "text": decision.Message})
			}
		}
	}); err != nil {
		hookFailure("loop completion hook", err)
	}
	if pending := session.Get("pending_title"); truthy(pending) {
		key := sessionKeyOr(session, sid)
		func() {
			defer func() { _ = recover() }()
			db, release := sessionDB(ctx, session)
			defer release()
			if db == nil {
				return
			}
			ok, err := db.SetSessionTitle(ctx, key, pyrepr.Str(pending))
			switch {
			case err != nil && isValueError(err):
				session.Set("pending_title", nil)
				logger().Info(fmt.Sprintf("Dropping pending title for session %s: %s", key, err))
			case err == nil && ok:
				session.Set("pending_title", nil)
			}
		}()
	}
	if text, ok := raw.(string); ok && st.ttsQueue == nil && pystr.Strip(text) != "" && voiceTTSEnabled() {
		go speakTextWithBarge(context.WithoutCancel(ctx), text)
	}
}

// Chains one follow up turn, releasing running and running onError when it fails
func dispatchFollowupTurn(ctx context.Context, rid any, sid string, session *tuigateway.Session, prompt any, what string, onDone, onError func()) {
	if err := catchPanic(func() {
		tuigateway.Emit(ctx, "message.start", sid, nil)
		runPromptSubmit(ctx, rid, sid, session, prompt, promptSubmitOptions{})
		if onDone != nil {
			onDone()
		}
	}); err != nil {
		if onError != nil {
			onError()
		}
		hookFailure(what, err)
		unlock := lockHistory(session)
		session.Set("running", false)
		unlock()
	}
}

// Chains what runs after running was released, a queued prompt first, then the goal, then notifications
func runPostTurnFollowups(ctx context.Context, rid any, sid string, session *tuigateway.Session, result map[string]any, goalFollowup string) {
	if steer, ok := result["pending_steer"].(string); ok && pystr.Strip(steer) != "" {
		unlock := lockHistory(session)
		enqueuePrompt(ctx, session, steer, session.Get("transport"), nil)
		unlock()
	}
	if drainQueuedPrompt(ctx, rid, sid, session) {
		return
	}
	if goalFollowup != "" {
		dispatch := false
		sessionTurnAdmission(session, func(admitted bool) {
			if !admitted || truthy(session.Get("running")) || truthy(session.Get("_turn_cancel_requested")) {
				return
			}
			session.Set("running", true)
			dispatch = true
		})
		if !dispatch {
			return
		}
		dispatchFollowupTurn(ctx, rid, sid, session, goalFollowup, "goal continuation dispatch", nil, nil)
	}
	if err := catchPanic(func() {
		registry := tools.DefaultProcessRegistry()
		key := strOr(session.Get("session_key"))
		owns := func(e map[string]any) bool {
			return tools.OwnsEvent(e, key, func(evt map[string]any) bool { return sessionOwnsNotificationEvent(ctx, sid, session, evt) }, e["type"] == "async_delegation")
		}
		scoped := sessionProfileRuntimeScope(ctx, session, false)
		registry.DrainedFor(owns, func(mine []map[string]any) {
			var deferred []map[string]any
			ready := []map[string]any{}
			for _, evt := range mine {
				if tools.ShouldSurfaceNotification(config.Tree(loadCfg(scoped)), evt) {
					ready = append(ready, evt)
				}
			}
			emitted, _ := session.SetDefault("_notification_emitted", map[string]bool{}).(map[string]bool)
			notifHandleReady(scoped, sid, session, ready, emitted, registry, tools.FormatProcessNotification, &deferred, true)
			for _, event := range deferred {
				registry.CompletionQueue.Put(event)
			}
		})
	}); err != nil {
		hookFailure("completion queue drain", err)
	}
}
