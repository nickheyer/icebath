package gatewayrun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Cancels the turn's background tasks, flushes the stream, aborts streaming TTS and releases the session slot
func (r *GatewayRunner) runAgentCleanupTurnTasks(ctx context.Context, tc *TurnContext, progressTask, logTask, interruptMonitor,
	notifyTask, trackingTask, streamTask *gatewayTask) {
	sessionKey := tc.sessionKey
	for _, task := range []*gatewayTask{progressTask, logTask, interruptMonitor, notifyTask} {
		if task != nil {
			task.Cancel()
		}
	}
	if streamTask != nil {
		if tc.streamConsumer() == nil {
			streamTask.Cancel()
			_, _ = streamTask.wait(context.WithoutCancel(ctx))
		} else {
			awaitStreamTask(context.WithoutCancel(ctx), streamTask)
		}
	}
	if stts := tc.streamingTTSConsumer(); stts != nil && !stts.Done() {
		stts.Abort("cleanup")
		suppressPanic(func() { stts.WaitComplete(2.0) })
	}
	trackingTask.Cancel()
	if sessionKey != "" {
		r.releaseRunningAgentState(ctx, sessionKey, tc.runGeneration)
	}
	if r.draining {
		draining := "draining"
		r.updateRuntimeStatus(ctx, &draining, nil)
	}
	for _, task := range []*gatewayTask{progressTask, logTask, interruptMonitor, trackingTask, notifyTask} {
		if task == nil {
			continue
		}
		if _, err := task.wait(context.WithoutCancel(ctx)); err != nil && !isCancellation(err) {
			runLog().Debug("background turn task failed during cleanup", "exc_info", err)
		}
	}
}

// Edits the stream consumer's message to the content, marking the response already sent when the edit lands
func (r *GatewayRunner) runAgentEditStreamedMessage(ctx context.Context, sc *gateway.GatewayStreamConsumer, source *gateway.SessionSource,
	response map[string]any, content, sessionKey, ok, failResult, failExc string) {
	res, err := sc.Adapter().EditMessage(ctx, source.ChatID, sc.MessageID(), content, true)
	if err != nil {
		runLog().Warn(fmt.Sprintf(failExc, sessionKey, err))
		return
	}
	if !res.Success {
		runLog().Warn(fmt.Sprintf(failResult, sessionKey, pyrepr.Str(res.Error)))
		return
	}
	response["already_sent"] = true
	runLog().Info(ok)
}

// Marks the response already sent when streaming delivered the exact final reply, reconciling a stale finalize by edit
func (r *GatewayRunner) runAgentMarkStreamedDelivery(ctx context.Context, response map[string]any, tc *TurnContext) {
	sc, source, sessionKey := tc.streamConsumer(), tc.source, tc.sessionKey
	if response == nil || pyrepr.Truthy(response["failed"]) {
		return
	}
	if finite, isBool := gateway.FiniteTurnRequired(ctx).(bool); isBool && finite {
		return
	}
	final, _ := response["final_response"].(string)
	isEmptySentinel := final == "" || final == "(empty)"
	previewed := pyrepr.Truthy(response["response_previewed"])
	contentDelivered := sc != nil && sc.FinalContentDelivered()
	staleFinalized := false
	if contentDelivered && !isEmptySentinel {
		suppressPanic(func() {
			matches := sc.DeliveredFinalMatches(final)
			staleFinalized = matches != nil && !*matches
		})
		if staleFinalized {
			contentDelivered = false
		}
	}
	transformed := pyrepr.Truthy(response["response_transformed"])
	streamed := runAgentStreamConfirmedFinalDelivery(sc, final)
	if isEmptySentinel {
		return
	}
	sk := logSessionKey(sessionKey)
	switch {
	case !transformed && (streamed || contentDelivered):
		runLog().Info(fmt.Sprintf("Suppressing normal final send for session %s: final delivery already confirmed (streamed=%s previewed=%s content_delivered=%s).",
			sk, pyrepr.Str(streamed), pyrepr.Str(previewed), pyrepr.Str(contentDelivered)))
		response["already_sent"] = true
	case !transformed && staleFinalized && sc != nil:
		scMsgID := sc.MessageID()
		switch {
		case sc.TurnSplitDelivery():
			runLog().Info(fmt.Sprintf("Stale streamed finalize detected for session %s on a multi-message split; skipping the in-place reconciliation edit and delivering the complete response via normal final send (#78541).", sk))
		case scMsgID != "" && scMsgID != "__no_edit__" && sc.Adapter() != nil:
			r.runAgentEditStreamedMessage(ctx, sc, source, response, final, sk,
				fmt.Sprintf("Reconciled stale streamed finalize for session %s: edited message %s with the complete response (#71643).", sk, scMsgID),
				"Stale-finalize reconciliation edit failed for session %s (%s); sending complete response via normal final send.",
				"Stale-finalize reconciliation edit failed for session %s: %s; sending complete response via normal final send.")
		default:
			runLog().Info(fmt.Sprintf("Stale streamed finalize detected for session %s with no editable message; delivering complete response via normal final send (#71643).", sk))
		}
	case transformed && sc != nil:
		if scMsgID := sc.MessageID(); scMsgID != "" {
			r.runAgentEditStreamedMessage(ctx, sc, source, response, pyrepr.Str(response["final_response"]), sk,
				fmt.Sprintf("Edited streamed message %s for session %s to include plugin-transformed content.", scMsgID, sk),
				"Transformed-final edit failed for session %s (%s); sending transformed response via normal final send.",
				"Failed to edit streamed message for session %s: %s")
		}
	case sc != nil && sc.StreamDeltasEnabled:
		runLog().Warn(fmt.Sprintf("Normal final-send NOT suppressed despite active stream consumer for session %s: "+
			"streamed=%s previewed=%s content_delivered=%s transformed=%s final_len=%d — "+
			"possible duplicate send (see wecom ack-timeout RCA).",
			sk, pyrepr.Str(streamed), pyrepr.Str(previewed), pyrepr.Str(contentDelivered), pyrepr.Str(transformed), utf8.RuneCountInString(final)))
	}
}

// Runs a coroutine on the loop from a callback, logging the reason when the loop is missing or refuses
func safeScheduleOnLoop(ctx context.Context, loop gateway.EventLoop, name string, fn func(ctx context.Context) (any, error), logMessage string) {
	if loop == nil {
		runLog().Debug(fmt.Sprintf("%s: loop is None", logMessage))
		return
	}
	taskCtx := context.WithoutCancel(ctx)
	if err := loop.CallSoonThreadsafe(func() { spawnGatewayTask(taskCtx, name, fn) }); err != nil {
		runLog().Debug(fmt.Sprintf("%s: %s", logMessage, err))
	}
}

// Registers deletion of the turn's temporary progress bubbles to run once the final response lands
func (r *GatewayRunner) runAgentScheduleBubbleCleanup(response map[string]any, cleanupAdapter gateway.PlatformAdapter, tc *TurnContext) {
	tc.holderMu.Lock()
	idsSnapshot := slices.Clone(tc.cleanupMsgIDs)
	tc.holderMu.Unlock()
	sessionKey := tc.sessionKey
	if !tc.cleanupProgress || cleanupAdapter == nil || len(idsSnapshot) == 0 || sessionKey == "" || response == nil || pyrepr.Truthy(response["failed"]) {
		return
	}
	chatIDSnapshot := tc.source.ChatID
	loopSnapshot := r.gatewayLoop
	cleanupTempBubbles := func(cbCtx context.Context) error {
		deleteAll := func(taskCtx context.Context) (any, error) {
			deleter, _ := cleanupAdapter.(gateway.MessageDeleter)
			for _, mid := range idsSnapshot {
				suppressPanic(func() { _, _ = deleter.DeleteMessage(taskCtx, chatIDSnapshot, mid) })
			}
			return nil, nil
		}
		suppressPanic(func() {
			safeScheduleOnLoop(cbCtx, loopSnapshot, "temp-bubble-cleanup", deleteAll, "Temp bubble cleanup scheduling error")
		})
		return nil
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug(fmt.Sprintf("Post-delivery cleanup registration failed: %s", pyrepr.Str(rec)))
			}
		}()
		cleanupAdapter.Base().RegisterPostDeliveryCallback(sessionKey, cleanupTempBubbles, tc.runGeneration)
	}()
}

// Publishes progress threading, status routing and the sync callback bridges onto the turn, returning the status thread metadata
func (r *GatewayRunner) runAgentBindTurnWiring(ctx context.Context, tc *TurnContext, turnRunner *TurnRunner, source *gateway.SessionSource,
	eventMessageID *string, nativeSlackTaskCards bool) (map[string]any, error) {
	progressMetadata, progressReplyTo, statusThreadMetadata := r.runAgentProgressThreading(ctx, source, eventMessageID, nativeSlackTaskCards)
	tc.progressMetadata, tc.progressReplyTo = progressMetadata, progressReplyTo
	tc.loopForStep = r.gatewayLoop
	tc.hooksRef = r.hooks
	tc.stepCallbackSync = turnRunner.stepCallbackSync
	tc.eventCallbackSync = turnRunner.eventCallbackSync
	tc.statusCallbackSync = turnRunner.statusCallbackSync
	statusAdapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	tc.statusAdapter = statusAdapter
	tc.statusChatID = source.ChatID
	tc.statusThreadMetadata = statusThreadMetadata
	return statusThreadMetadata, nil
}

// Status detail of the long running heartbeat, the iteration counter and current action, empty when unreadable
func longRunningStatusDetail(ctx context.Context, activity map[string]any, wantIterationDetail bool) string {
	if len(activity) == 0 {
		return ""
	}
	var parts []string
	if wantIterationDetail {
		apiCalls, hasCalls := activity["api_call_count"]
		maxIterations, hasMax := activity["max_iterations"]
		if !hasCalls || !hasMax {
			return ""
		}
		progress, err := agent.FormatIterationProgress(ctx, apiCalls, maxIterations)
		if err != nil {
			return ""
		}
		parts = append(parts, progress)
	}
	action := activity["current_tool"]
	if !pyrepr.Truthy(action) {
		action = activity["last_activity_desc"]
	}
	if pyrepr.Truthy(action) {
		parts = append(parts, pyrepr.Str(action))
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, ", ")
}

// Periodic still working heartbeat edited in place where supported, until the run loses its slot or finishes
func (r *GatewayRunner) runAgentNotifyLongRunning(ctx context.Context, disp *runAgentDisplay, tc *TurnContext, executorTask *atomic.Pointer[gatewayTask]) error {
	notifyStart := time.Now()
	interval := floatEnv("ICEBATH_AGENT_NOTIFY_INTERVAL", 180)
	longRunningMode := disp.displaySurfaceMode("long_running_notifications", true, nil, true)
	if interval <= 0 || longRunningMode == "off" {
		return nil
	}
	source, sessionKey := tc.source, tc.sessionKey
	statusThreadMetadata := tc.statusThreadMetadata
	notifyAdapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if notifyAdapter == nil {
		return nil
	}
	shouldEmit := func() bool {
		var checker doneChecker
		if task := executorTask.Load(); task != nil {
			checker = task
		}
		return r.shouldEmitLongRunningNotification(sessionKey, tc.currentAgent(), checker)
	}
	heartbeatMsgID := ""
	for {
		select {
		case <-ctx.Done():
			return errTaskCancelled
		case <-time.After(time.Duration(interval * float64(time.Second))):
		}
		if !shouldEmit() {
			return nil
		}
		elapsedMins := int(time.Since(notifyStart).Seconds()) / 60
		wantIterationDetail := pyrepr.Truthy(disp.resolveDisplaySetting(ctx, disp.userConfig, disp.platformKey, "busy_ack_detail", true))
		statusDetail := ""
		suppressPanic(func() {
			statusDetail = longRunningStatusDetail(ctx, agentActivitySummary(tc.currentAgent()), wantIterationDetail)
		})
		var heartbeatText string
		if longRunningMode == "generic" {
			heartbeatText = disp.genericStatusPhrase("status")
		} else {
			heartbeatText = i18n.T(ctx, "gateway.progress.working_heartbeat", "", map[string]any{"minutes": elapsedMins, "detail": statusDetail})
		}
		var notifyRes *gateway.SendResult
		if heartbeatMsgID != "" {
			res, err := notifyAdapter.EditMessage(ctx, source.ChatID, heartbeatMsgID, heartbeatText, false)
			if err != nil {
				runLog().Debug(fmt.Sprintf("Heartbeat edit failed: %s", err))
			} else {
				notifyRes = &res
			}
		}
		if notifyRes != nil && notifyRes.Success {
			continue
		}
		if !shouldEmit() {
			return nil
		}
		res, err := notifyAdapter.Send(ctx, source.ChatID, heartbeatText, "", interimMetadata(nonConversationalMetadata(statusThreadMetadata, source.Platform)))
		if err != nil {
			runLog().Debug(fmt.Sprintf("Long-running notification error: %s", err))
			continue
		}
		if res.Success && pyrepr.Truthy(res.MessageID) {
			heartbeatMsgID = pyrepr.Str(res.MessageID)
			if tc.cleanupProgress {
				tc.holderMu.Lock()
				tc.cleanupMsgIDs = append(tc.cleanupMsgIDs, heartbeatMsgID)
				tc.holderMu.Unlock()
			}
		}
	}
}
