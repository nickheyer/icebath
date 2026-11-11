package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Text of the first n runes with newlines flattened, the log preview of a message
func logPreview(text string, n int) string {
	runes := []rune(text)
	return strings.ReplaceAll(string(runes[:min(len(runes), n)]), "\n", " ")
}

// Inner handler that runs one turn under the running agents sentinel guard and returns the reply to send
func (r *GatewayRunner) handleMessageWithAgent(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string, runGeneration int) (*string, error) {
	msgStartTime := time.Now()
	platformName := string(source.Platform)
	who := "unknown"
	if source.UserName != nil && *source.UserName != "" {
		who = *source.UserName
	} else if source.UserID != nil && *source.UserID != "" {
		who = *source.UserID
	}
	chat := source.ChatID
	if chat == "" {
		chat = "unknown"
	}
	replyToText := ""
	if event.ReplyToText != nil {
		replyToText = *event.ReplyToText
	}
	runLog().Info(fmt.Sprintf("inbound message: platform=%s user=%s chat=%s msg=%s reply_to_id=%s reply_to_text=%s",
		platformName, who, chat, pyrepr.Repr(logPreview(event.Text, 80)), optText(event.ReplyToMessageID), pyrepr.Repr(logPreview(replyToText, 80))))
	source, entry, sessionKey, err := r.hmwaResolveSession(ctx, event, source)
	if err != nil || entry == nil {
		return nil, err
	}
	prepared, reply, sessionEnv, err := r.hmwaPrepareTurn(ctx, event, source, entry, sessionKey, quickKey, runGeneration)
	if err != nil {
		if sessionEnv != nil {
			r.clearSessionEnv(sessionEnv)
		}
		return nil, err
	}
	if prepared == nil {
		return reply, nil
	}
	defer r.clearSessionEnv(sessionEnv)
	var hookCtx map[string]any
	result, err := func() (*string, error) {
		threadID := ""
		if source.ThreadID != nil && *source.ThreadID != "" {
			threadID = *source.ThreadID
		}
		messageRunes := []rune(pyrepr.Str(prepared.messageText))
		hookCtx = map[string]any{
			"platform": string(source.Platform), "user_id": optAny(source.UserID), "chat_id": source.ChatID,
			"thread_id": threadID, "chat_type": source.ChatType, "session_id": entry.SessionID,
			"message": string(messageRunes[:min(len(messageRunes), 500)]),
		}
		if err := r.hooks.emit(ctx, "agent:start", hookCtx); err != nil {
			return nil, err
		}
		if !heartbeatOwnerIsCurrent(ctx, r, event, sessionKey) {
			return nil, nil
		}
		runStartSessionID := entry.SessionID
		turnStarted := pythread.Monotonic()
		event.SetHeartbeatExecutionStarted(true)
		turnChannelPrompt, turnSource := r.pinnedChannelInputs(sessionKey, event.ChannelPrompt, source, event.Internal || event.InheritChannelPin)
		if !event.Internal {
			r.persistPromptPins(ctx, sessionKey, &runStartSessionID)
		}
		var inboundMessageID *string
		if event.MessageID != nil && *event.MessageID != "" {
			id := *event.MessageID
			inboundMessageID = &id
		}
		displayMetadata := map[string]any{"gateway_input_owner": optAny(prepared.persistenceOwner)}
		maps.Copy(displayMetadata, gateway.PersistedUserMetadataForEvent(ctx, gateway.DisplayEventOf(event)))
		maps.Copy(displayMetadata, gateway.ReplyExpectedMetadata(event.ReplyExpected))
		maps.Copy(displayMetadata, config.DiagnosticMetadata(event))
		messageText, _ := prepared.messageText.(string)
		var persistUserTimestamp *float64
		if ts, ok := prepared.persistUserTimestamp.(float64); ok {
			persistUserTimestamp = &ts
		}
		agentResult, err := r.runAgent(ctx, runAgentParams{
			message: messageText, contextPrompt: prepared.contextPrompt, history: prepared.history, source: turnSource,
			sessionID: runStartSessionID, sessionKey: sessionKey, runGeneration: &runGeneration,
			eventMessageID: gateway.ReplyAnchorForEvent(event), inboundMessageID: inboundMessageID,
			channelPrompt: turnChannelPrompt, titleUserMessage: prepared.titleUserMessage,
			persistUserMessage: prepared.persistUserMessage, persistUserTimestamp: persistUserTimestamp,
			persistUserDisplayKind: displayKindPointer(prepared.persistUserDisplayKind), replyExpected: event.ReplyExpected,
			persistUserDisplayMetadata: displayMetadata, messageType: event.MessageType,
			scheduledHeartbeat: event.HeartbeatSessionID() != nil && *event.HeartbeatSessionID() != "",
			sessionEnv:         sessionEnv,
		})
		if err != nil {
			return nil, err
		}
		turnSeconds := pythread.Monotonic() - turnStarted
		if agentResult != nil {
			if terminal := agentResult["queued_terminal_inbound_id"]; pyrepr.Truthy(terminal) {
				id := pyrepr.Str(terminal)
				event.LedgerMessageID = &id
			}
			if category, ok := agentResult["queued_terminal_notification_category"]; ok {
				if event.Metadata == nil {
					event.Metadata = map[string]any{}
				}
				event.Metadata["notification_category"] = category
			}
			if muted, ok := agentResult["_notification_reply_muted"].(bool); ok {
				event.SetNotificationReplyMuted(muted)
			}
		}
		r.hmwaStopTypingForTurn(ctx, event, source)
		if !r.isSessionRunCurrent(quickKey, runGeneration) {
			r.hmwaDiscardStaleResult(ctx, source, quickKey, runGeneration)
			return nil, nil
		}
		response, silent, agentMessages, err := r.hmwaShapeAgentResponse(ctx, agentResult, source, prepared.history, entry, sessionKey,
			quickKey, runGeneration, runStartSessionID, platformName, msgStartTime, prepared.persistUserDisplayKind, event.ReplyExpected)
		if err != nil {
			return nil, err
		}
		withReasoning, err := r.hmwaPrependReasoning(ctx, agentResult, response, source, silent)
		if err != nil {
			return nil, err
		}
		response = usageWindowChatWording(agentResult, withReasoning)
		footerLine := r.hmwaRuntimeFooterLine(ctx, agentResult, source, turnSeconds)
		if footerLine != "" && response != "" && !pyrepr.Truthy(agentResult["already_sent"]) && !silent {
			response = response + "\n\n" + footerLine
		}
		if err := r.hmwaPostTurnHooks(ctx, hookCtx, agentResult, response); err != nil {
			return nil, err
		}
		failedEarly, hiddenIncomplete, overflow := hmwaClassifyTurnFailure(agentResult, prepared.history, entry)
		if failedEarly && !overflow {
			response = hmwaAddFailedTurnNotice(response, hmwaFailedTurnNotice(agentResult))
		}
		response, entry, err = r.hmwaCompressionExhaustionReset(ctx, agentResult, response, entry, sessionKey, source)
		if err != nil {
			return nil, err
		}
		if err := r.hmwaPersistTurnTranscript(ctx, event, source, entry, sessionKey, agentResult, agentMessages, prepared,
			response, failedEarly, hiddenIncomplete, overflow); err != nil {
			return nil, err
		}
		r.recordRoomSilenceDecision(ctx, sessionKey, agentResult, silent, prepared.persistUserDisplayKind, event.ReplyExpected)
		return r.deliverTurnResponseThenLimitOffer(ctx, event, source, entry, sessionKey, runGeneration, agentResult, agentMessages, response, footerLine, silent)
	}()
	if err != nil && ctx.Err() == nil {
		reply := r.hmwaAgentErrorReply(ctx, err, event, source, entry, sessionKey, prepared)
		return &reply, nil
	}
	return result, err
}
