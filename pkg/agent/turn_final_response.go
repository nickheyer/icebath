package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var repetitionStopped = RepetitionCopy("before delivery", "so the repeated output was discarded.", "; refusing to return a")

var ephemeralScaffoldingFlags = []string{"_thinking_prefill", "_empty_recovery_synthetic", "_empty_terminal_sentinel", "_dropped_toolcall_nudge"}

// SetSilenceMarkerNeedsReply is the gateway turn runner switch that forbids a bare silence marker after a written reply
func (a *AIAgent) SetSilenceMarkerNeedsReply(v bool) { a.silenceMarkerNeedsReply = v }

// True when a bare silence marker should yield to the reply written beside a housekeeping round
func bareMarkerAfterReply(a *AIAgent, final string, truncatedParts []TruncatedPart, promoted string) bool {
	if !a.silenceMarkerNeedsReply || len(truncatedParts) > 0 || promoted != "" || a.interruptRequested {
		return false
	}
	reply := housekeepingReplyLine(a)
	return reply != nil && *reply != "" && !gateway.IsIntentionalSilenceResponse(*reply) &&
		gateway.IsIntentionalSilenceResponse(pystr.Strip(a.stripThinkBlocks(final)))
}

// Ends the turn on the completed housekeeping reply instead of its bare silence marker, nil otherwise
func recoverMarkerReply(a *AIAgent, s *LoopState, final string, promoted string) *PhaseVerdict {
	if !bareMarkerAfterReply(a, final, s.TruncatedResponseParts, promoted) {
		return nil
	}
	s.FinalResponse = nil
	if reply := takeHousekeepingReplyLine(a); reply != nil {
		s.FinalResponse = *reply
	}
	s.TurnExitReason = "fallback_prior_turn_content"
	conversationLoopLog().Info("Silence marker after a written reply: using that reply as the final response")
	return &PhaseVerdict{Action: "break"}
}

// Stitches length continuation fragments before the closing piece and unflags the fragment rows
func absorbContinuationFragments(final string, parts []TruncatedPart, messages []*state.Message) (string, bool) {
	if len(parts) == 0 {
		return final, false
	}
	for _, m := range messages {
		if m != nil && m.Extra != nil {
			delete(m.Extra, "_length_continuation_fragment")
			delete(m.Extra, "_length_continuation_nudge")
		}
	}
	return joinTruncatedParts(append(append([]TruncatedPart(nil), parts...), TruncatedPart{Text: final})), true
}

// True when a reasoning detail is a signed Anthropic thinking block or a native assistant carrier
func hasSignedThinkingDetail(resp *provider.Response) bool {
	for _, d := range reasoningDetailsOf(resp) {
		detail, ok := d.(map[string]any)
		if !ok {
			continue
		}
		kind := detail["type"]
		if (kind == "thinking" || kind == "redacted_thinking") && (pyTruthy(detail["signature"]) || pyTruthy(detail["data"])) {
			return true
		}
		if kind != nil && strings.HasSuffix(pyrepr.Str(kind), ".native_assistant") {
			return true
		}
	}
	return false
}

// Assistant rows carrying tool calls
func toolTurnCount(messages []*state.Message) int {
	n := 0
	for _, m := range messages {
		if m != nil && m.Role == "assistant" && len(m.ToolCalls) > 0 {
			n++
		}
	}
	return n
}

// Finishes or defers a text only response, flushing the final only after the stop gates accept it
func finishTextResponse(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	msg, finishReason := s.AssistantMessage, s.FinishReason
	var content any
	toolCalls := 0
	if msg != nil && msg.Message != nil {
		content, toolCalls = msg.Message.Content, len(msg.Message.ToolCalls)
	}
	text, isText := content.(string)
	promoted := ""
	if finishReason == "stop" && toolCalls == 0 && (content == nil || (isText && pystr.Strip(text) == "")) &&
		!hasSignedThinkingDetail(msg) && answerInReasoningCapability(ctx, a) {
		if promoted = a.extractReasoning(msg); promoted != "" {
			conversationLoopLog().Warn(fmt.Sprintf("Reasoning-only clean stop (%d chars) — returning the reasoning as the final response (model=%s provider=%s api_calls=%d tool_turns=%d)",
				pyLen(promoted), a.model, a.provider, s.APICallCount, toolTurnCount(s.Messages)))
		}
	}
	final := promoted
	if final == "" && pyTruthy(content) {
		final = text
		if !isText {
			final = state.ContentText(content)
		}
	}
	a.mutePostResponse = false

	if !a.hasContentAfterThinkBlock(final) {
		return recoverEmptyResponse(ctx, a, s, msg, finishReason, final)
	}
	a.emptyContentRetries, a.thinkingPrefillRetries = 0, 0
	a.emitPendingFallbackNotice()
	a.clearStatusBuffer()
	if v := recoverMarkerReply(a, s, final, promoted); v != nil {
		return *v, nil
	}

	ackMode := intentAckContinuationMode(a)
	stallText := a.stripThinkBlocks(final)
	stallContinue := a.stallGuards && len(a.validToolNames) > 0 && s.CodexAckContinuations < 2 &&
		(trailingContinueIntent(stallText) || (promoted != "" && promotedReasoningAnnouncesAction(stallText)))
	toolRows := toolResultsThisTurn(s.Messages)
	degenerate := a.stallGuards && ackMode != "off" && s.CodexAckContinuations < 2 && toolRows > 0 &&
		looksLikeDegenerateFinal(stallText, s.UserMessage)
	kind := ""
	switch {
	case stallContinue:
		kind = "stall"
	case degenerate:
		kind = "degenerate"
	case ackMode != "off" && len(a.validToolNames) > 0 && s.CodexAckContinuations < 2 &&
		a.looksLikeCodexIntermediateAck(s.UserMessage, final, s.Messages, ackMode == "codex_only"):
		kind = "ack"
	}
	if kind != "" {
		switch kind {
		case "stall":
			conversationLoopLog().Info(fmt.Sprintf("Stall guard: turn ending on trailing continue-intent with no tool calls — re-prompting to act (%d/2)", s.CodexAckContinuations+1))
		case "degenerate":
			head := []rune(stallText)
			conversationLoopLog().Warn(fmt.Sprintf("Degenerate final: %d-char fragment %s ended the turn after %d tool result(s) — re-prompting (%d/2)",
				len(head), pyrepr.Repr(string(head[:min(len(head), 40)])), toolRows, s.CodexAckContinuations+1))
		}
		s.CodexAckContinuations++
		interim := a.buildAssistantMessage(msg, "incomplete")
		if promoted != "" {
			interim["api_content"] = final
		}
		a.emitInterimAssistantMessage(appendLiveMessage(&s.Messages, interim))
		nudge := codexAckContinuationNudge
		if kind == "degenerate" {
			nudge = degenerateFinalNudge
		}
		appendLiveMessage(&s.Messages, map[string]any{"role": "user", "content": nudge})
		a.sessionMessages = s.Messages
		s.FinalResponse = nil
		return PhaseVerdict{Action: "continue"}, nil
	}
	s.CodexAckContinuations = 0

	final, joined := absorbContinuationFragments(final, s.TruncatedResponseParts, s.Messages)
	if joined {
		s.TruncatedResponseParts, s.LengthContinueRetries = []TruncatedPart{}, 0
	}
	final = pystr.Strip(a.stripThinkBlocks(final))
	s.FinalResponse = final

	if final != "" && pyLen(final) >= provider.StopPathMinChars && provider.IsRunawayRepetition(final) {
		line, userResponse, errText := repetitionStopped[0], repetitionStopped[1], repetitionStopped[2]
		a.vprint(a.logPrefix+line, true, true)
		a.cleanupTaskResources(s.EffectiveTaskID)
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "return", Result: StampFailure(PartialResult(s.Messages, s.APICallCount, userResponse, &errText, false, false), "truncated", true)}, nil
	}

	finalMsg := a.buildAssistantMessage(msg, finishReason)
	if promoted != "" {
		finalMsg["api_content"] = final
	}
	if finishReason == "tool_calls" && toolCalls == 0 && a.droppedToolcallRetries < 3 {
		a.droppedToolcallRetries++
		conversationLoopLog().Warn(fmt.Sprintf("finish_reason=tool_calls with empty tool_calls array (narration only) — re-prompting to emit the call (retry %d/3, model=%s provider=%s)",
			a.droppedToolcallRetries, a.model, a.provider))
		a.emitDiagnosticStatus(fmt.Sprintf("↻ Model signaled a tool call but sent none — re-prompting (%d/3)", a.droppedToolcallRetries))
		finalMsg["_dropped_toolcall_nudge"] = true
		appendLiveMessage(&s.Messages, finalMsg)
		appendLiveMessage(&s.Messages, map[string]any{"role": "user", "content": droppedToolcallNudgeContent, "_dropped_toolcall_nudge": true})
		a.sessionMessages = s.Messages
		s.FinalResponse = nil
		return PhaseVerdict{Action: "continue"}, nil
	}
	a.droppedToolcallRetries = 0

	for n := len(s.Messages); n > 0 && s.Messages[n-1] != nil && anyFlagSet(s.Messages[n-1].Extra, ephemeralScaffoldingFlags); n = len(s.Messages) {
		s.Messages = s.Messages[:n-1]
	}

	gate := applyStopGates(ctx, a, finalMsg, final, s)
	s.PendingVerificationResponse, s.PendingVerificationResponsePreviewed = gate.pendingVerificationResponse, gate.pendingVerificationResponsePreviewed
	if gate.continueTurn {
		s.FinalResponse = gate.finalResponse
		return PhaseVerdict{Action: "continue"}, nil
	}

	transformed := false
	if !a.interruptRequested {
		final, transformed, _ = applyLLMOutputTransform(ctx, a, final, a.currentTurnID, nil)
	}
	if transformed {
		if promoted != "" {
			finalMsg["api_content"] = final
		} else {
			finalMsg["content"] = final
		}
	}
	publishHeldFinalRow(a, finalMsg, final, joined)
	appendLiveMessage(&s.Messages, finalMsg)
	if _, err := a.flushLiveMessages(s.Messages, s.ConversationHistory); err != nil {
		session := a.sessionID
		if session == "" {
			session = "none"
		}
		conversationLoopLog().Warn(fmt.Sprintf("final text-turn flush failed (session=%s) — reply is not yet durable; relying on finalize_turn retry", session), "error", err)
	}
	s.TurnExitReason = fmt.Sprintf("text_response(finish_reason=%s)", finishReason)
	if !a.quietMode {
		a.safePrint(fmt.Sprintf("🎉 Conversation completed after %d OpenAI-compatible API call(s)", s.APICallCount), false)
	}
	s.FinalResponse = final
	return PhaseVerdict{Action: "break"}, nil
}
