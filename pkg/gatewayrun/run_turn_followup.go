package gatewayrun

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Recursion cap of interrupt and queued follow up turns before the message is queued instead
const maxInterruptDepth = 3

// Integer value of a result field as Python isinstance int reads it, bool included
func pythonInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Follow up result carrying the outer history offset so outer persistence keeps every queued turn
func preserveQueuedFollowupHistoryOffset(currentResult, followupResult map[string]any) map[string]any {
	if followupResult == nil || currentResult == nil {
		return followupResult
	}
	currentOffset, ok := pythonInt(currentResult["history_offset"])
	if !ok {
		return followupResult
	}
	if followupOffset, isInt := pythonInt(followupResult["history_offset"]); isInt && followupOffset <= currentOffset {
		return followupResult
	}
	out := maps.Clone(followupResult)
	out["history_offset"] = currentResult["history_offset"]
	return out
}

// Persisted display kind of a turn as the turn context holds it, none for a non string kind
func displayKindPointer(kind any) *string {
	if s, ok := kind.(string); ok {
		return &s
	}
	return nil
}

// Session key or the question mark the runner logs for a missing one
func logSessionKey(sessionKey string) string {
	if sessionKey == "" {
		return "?"
	}
	return sessionKey
}

// Delivers the first response before a queued follow up runs unless streaming already did, then fires deferred callbacks
func (r *GatewayRunner) runAgentDeliverFirstResponse(ctx context.Context, tc *TurnContext, adapter gateway.PlatformAdapter,
	response, result map[string]any, streamTask *gatewayTask) {
	if tc.muteNotificationReply {
		return
	}
	sessionKey := tc.sessionKey
	sc := tc.streamConsumer()
	if sc != nil && streamTask != nil {
		awaitStreamTask(ctx, streamTask)
	}
	deliveryResult := response
	if deliveryResult == nil {
		deliveryResult = result
		if len(deliveryResult) == 0 {
			deliveryResult = map[string]any{}
		}
	}
	firstResponse, _ := deliveryResult["final_response"].(string)
	alreadyStreamed := runAgentStreamConfirmedFinalDelivery(sc, firstResponse)
	silent := false
	displayKind := optAny(tc.persistUserDisplayKind)
	anchor, inboundID, replyExpected := r.turnReplyAnchors(tc)
	if isIntentionalSilence(deliveryResult, firstResponse) {
		silent = r.silenceAllowedForSource(ctx, tc.source, displayKind, replyExpected, deliveryResult)
		if silent {
			runLog().Info(fmt.Sprintf("Queued follow-up for session %s: suppressing intentional silence marker before continuing.", logSessionKey(sessionKey)))
			firstResponse = ""
		} else {
			runLog().Warn(fmt.Sprintf("Queued follow-up for session %s: replacing a human-turn silence marker.", logSessionKey(sessionKey)))
			firstResponse = unexpectedSilenceReply(ctx)
			alreadyStreamed = false
		}
	}
	r.recordRoomSilenceDecision(ctx, sessionKey, deliveryResult, silent, displayKind, replyExpected)
	deliverMedia := !pyrepr.Truthy(deliveryResult["failed"])
	if firstResponse != "" {
		if alreadyStreamed {
			runLog().Info(fmt.Sprintf("Queued follow-up for session %s: final text delivery confirmed; delivering explicit media before continuing.", logSessionKey(sessionKey)))
		} else {
			runLog().Info(fmt.Sprintf("Queued follow-up for session %s: final stream delivery not confirmed; sending first response before continuing.", logSessionKey(sessionKey)))
		}
		var consumer queuedStreamConsumer
		if sc != nil {
			consumer = sc
		}
		textDelivered, err := func() (delivered bool, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					delivered, err = false, fmt.Errorf("%s", pyrepr.Str(rec))
				}
			}()
			return r.deliverQueuedFirstResponse(ctx, firstResponse, tc.source, adapter, tc.statusThreadMetadata, anchor,
				alreadyStreamed, deliverMedia, consumer, sessionKey, inboundID)
		}()
		if err != nil {
			runLog().Warn(fmt.Sprintf("Failed to send first response before queued message: %s", err))
		} else if textDelivered && result != nil {
			result["already_sent"] = true
			result["media_already_delivered"] = deliverMedia
		}
	}
	if callback := popPostDeliveryCallback(adapter, sessionKey, tc.runGeneration); callback != nil {
		suppressPanic(func() { _ = callback(ctx) })
	}
}

// Runs the queued or interrupting follow up as the next turn through a recursive agent run
func (r *GatewayRunner) runAgentQueuedFollowup(ctx context.Context, tc *TurnContext, adapter gateway.PlatformAdapter, pending string,
	pendingEvent *gateway.MessageEvent, response, result map[string]any, streamTask *gatewayTask) (map[string]any, error) {
	source, sessionID, sessionKey, runGeneration := tc.source, tc.sessionID, tc.sessionKey, tc.runGeneration
	interruptDepth, history, statusThreadMetadata := tc.interruptDepth, tc.history, tc.statusThreadMetadata
	runLog().Debug(fmt.Sprintf("Processing pending message: '%s...'", headRunes(pending, 40)))
	if adapter != nil && sessionKey != "" {
		if active := adapter.Base().ActiveSessionEvent(sessionKey); active != nil {
			active.Clear()
		}
	}
	if interruptDepth >= maxInterruptDepth {
		runLog().Warn(fmt.Sprintf("Interrupt recursion depth %d reached for session %s — queueing message instead of recursing.", interruptDepth, sessionKey))
		if err := r.requeueAtDepthCap(ctx, sessionKey, source, pendingEvent, pending); err != nil {
			return nil, err
		}
		if held := tc.result(); len(held) > 0 {
			return held, nil
		}
		var finalResponse any
		if response != nil {
			finalResponse = response
		}
		return map[string]any{"final_response": finalResponse, "messages": history}, nil
	}
	if !pyrepr.Truthy(result["interrupted"]) {
		r.runAgentDeliverFirstResponse(ctx, tc, adapter, response, result, streamTask)
	}
	updatedHistory := history
	if messages, ok := result["messages"]; ok {
		updatedHistory = resultRows(messages)
	}
	nextSource, nextMessage, nextSessionKey := source, pending, sessionKey
	var nextMessageID, nextChannelPrompt, nextInboundID *string
	var nextMessageType gateway.MessageType
	var nextPersistMessage any
	nextDisplayKind := gateway.DisplayKindForEvent(ctx, gateway.DisplayEventOf(pendingEvent))
	var nextReplyExpected *bool
	if pendingEvent != nil {
		_, _, nextReplyExpected = pendingEvent.ReplyFields()
		if pendingEvent.Source != nil {
			nextSource = pendingEvent.Source
		}
		if isGoalContinuationEvent(pendingEvent) && !r.goalStillActiveForSession(ctx, sessionID) {
			runLog().Info(fmt.Sprintf("Discarding stale goal continuation for session %s — goal is no longer active", logSessionKey(sessionKey)))
			return result, nil
		}
		if key, err := r.sessionKeyForSource(ctx, nextSource); err != nil {
			runLog().Debug(fmt.Sprintf("Queued follow-up session-key resolution failed; reusing %s", logSessionKey(sessionKey)), "exc_info", err)
		} else {
			nextSessionKey = key
		}
		prepared, err := r.prepareProfileScopedInboundMessageText(ctx, pendingEvent, nextSource, nextSessionKey)
		if err != nil {
			return nil, err
		}
		if prepared == nil {
			return result, nil
		}
		nextMessage = *prepared
		nextPersistMessage = stripDiscordTriggeringNote(pendingEvent, nextMessage)
		nextMessageID = gateway.ReplyAnchorForEvent(pendingEvent)
		if pendingEvent.MessageID != nil && *pendingEvent.MessageID != "" {
			inboundID := *pendingEvent.MessageID
			nextInboundID = &inboundID
		}
		nextChannelPrompt, nextSource = r.pinnedChannelInputs(nextSessionKey, pendingEvent.ChannelPrompt, nextSource,
			pendingEvent.Internal || pendingEvent.InheritChannelPin)
		if !pendingEvent.Internal {
			r.persistPromptPins(ctx, nextSessionKey, &sessionID)
		}
		nextMessageType = pendingEvent.MessageType
	} else {
		nextChannelPrompt = tc.channelPrompt
	}
	clearAdapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	if clearAdapter != nil && sessionKey != "" && runGeneration != nil {
		gateway.DiscardStreamingTTSCompletedTurn(clearAdapter, sessionKey, *runGeneration)
	}
	if clearAdapter != nil {
		suppressPanic(func() {
			_ = clearAdapter.SendTyping(ctx, source.ChatID, gateway.TypingMetadataForTurn(clearAdapter, source.ChatID, nextMessageID, statusThreadMetadata))
		})
	}
	var hookAdapter gateway.PlatformAdapter
	if pendingEvent != nil {
		if hookAdapter, err = r.intakeAdapterFor(ctx, nextSource); err != nil {
			return nil, err
		}
	}
	gateway.RunFollowupProcessingHook(ctx, hookAdapter, pendingEvent, "on_processing_start", "")
	displayMetadata := gateway.DisplayMetadataForEvent(ctx, gateway.DisplayEventOf(pendingEvent))
	maps.Copy(displayMetadata, gateway.ReplyExpectedMetadata(nextReplyExpected))
	if pendingEvent != nil {
		maps.Copy(displayMetadata, config.DiagnosticMetadata(pendingEvent))
	}
	if len(displayMetadata) == 0 {
		displayMetadata = nil
	}
	followupResult, err := func() (map[string]any, error) {
		defer func() {
			if rec := recover(); rec != nil {
				gateway.RunFollowupProcessingHook(context.WithoutCancel(ctx), hookAdapter, pendingEvent, "on_processing_complete", gateway.ProcessingFailure)
				panic(rec)
			}
		}()
		r.refreshAgentCacheMessageCount(ctx, sessionKey, sessionID)
		return r.runAgent(ctx, runAgentParams{
			message: nextMessage, contextPrompt: tc.contextPrompt, history: updatedHistory, source: nextSource,
			sessionID: sessionID, sessionKey: nextSessionKey, runGeneration: runGeneration, interruptDepth: interruptDepth + 1,
			eventMessageID: nextMessageID, inboundMessageID: nextInboundID, channelPrompt: nextChannelPrompt,
			messageType: nextMessageType, persistUserMessage: nextPersistMessage,
			persistUserDisplayKind: displayKindPointer(nextDisplayKind), replyExpected: nextReplyExpected,
			persistUserDisplayMetadata: displayMetadata, sessionEnv: tc.sessionEnv,
		})
	}()
	if err != nil {
		hookCtx := context.WithoutCancel(ctx)
		outcome := gateway.ProcessingFailure
		if isCancellation(err) || ctx.Err() != nil {
			outcome = gateway.FollowupCancelOutcome(ctx, hookAdapter)
		}
		gateway.RunFollowupProcessingHook(hookCtx, hookAdapter, pendingEvent, "on_processing_complete", outcome)
		return nil, err
	}
	gateway.RunFollowupProcessingHook(ctx, hookAdapter, pendingEvent, "on_processing_complete", gateway.ProcessingSuccess)
	merged := preserveQueuedFollowupHistoryOffset(result, followupResult)
	if merged != nil {
		if _, present := merged["queued_terminal_inbound_id"]; !present {
			category := any("result")
			if pendingEvent != nil && pendingEvent.Internal {
				if v, ok := pendingEvent.Metadata["notification_category"]; ok {
					category = v
				}
			}
			terminal := maps.Clone(merged)
			terminal["queued_terminal_inbound_id"] = optAny(nextInboundID)
			terminal["queued_terminal_display_kind"] = nextDisplayKind
			terminal["queued_terminal_reply_expected"] = nil
			if nextReplyExpected != nil {
				terminal["queued_terminal_reply_expected"] = *nextReplyExpected
			}
			terminal["queued_terminal_source"] = nextSource
			terminal["queued_terminal_notification_category"] = category
			merged = terminal
		}
	}
	return merged, nil
}
