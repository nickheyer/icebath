package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var inlineThinkRe = regexp.MustCompile(`(?i)<think>|<thinking>|<reasoning>`)

// Budgeted empty response retry returning the loop action or empty, the interrupt result and whether the empties are deterministic
func retryEmpty(ctx context.Context, a *AIAgent, s *LoopState, response *provider.Response, finishReason string, emptyCandidate, observedGeneration bool) (string, map[string]any, bool, error) {
	if emptyCandidate {
		a.recordEmptyAttempt(ctx, finishReason, response, observedGeneration)
	}
	budget := defaultEmptyRetryBudget
	if emptyCandidate {
		budget = a.emptyRetryBudget(ctx, response)
	}
	deterministic := emptyCandidate && a.deterministicEmpty()
	if !(emptyCandidate && a.emptyContentRetries < budget && !deterministic) {
		return "", nil, deterministic, nil
	}
	a.emptyContentRetries++
	n := a.emptyContentRetries
	wait := provider.JitteredBackoff(n, provider.BackoffOptions{BaseDelay: 5 * time.Second, MaxDelay: 60 * time.Second}).Seconds()
	conversationLoopLog().Warn(fmt.Sprintf("Empty response (no content or reasoning) — retry %d/%d in %.1fs (model=%s)", n, budget, wait, a.model))
	note := ""
	if budget < defaultEmptyRetryBudget {
		note = " — high-cost request, reduced retry budget"
	}
	a.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Empty response from model — retrying (%d/%d) in %.0fs%s", n, budget, wait, note))
	interrupted, err := interruptibleBackoffSleep(ctx, a, wait, nil, &s.Messages, s.ConversationHistory, s.APICallCount,
		"Interrupt detected during empty-response retry wait, aborting.",
		fmt.Sprintf("Operation interrupted: retrying empty response from model (retry %d/%d).", n, budget),
		fmt.Sprintf("empty response retry backoff (%d/%d)", n, budget))
	if err != nil {
		return "", nil, deterministic, err
	}
	if interrupted != nil {
		return "return", interrupted, deterministic, nil
	}
	return "continue", nil, deterministic, nil
}

// Persists the empty sentinel row once retries and fallback are exhausted and returns the delivery text
func terminalEmpty(a *AIAgent, s *LoopState, assistantMessage *provider.Response, finishReason string) string {
	if cost := a.streakCostUSD(); cost != nil {
		usd, _ := cost.Float64()
		a.BufferDiagnosticStatus(fmt.Sprintf("ℹ️ Estimated cost of these empty attempts: ~$%.2f (input tokens are billed per attempt even when no answer is produced)", usd))
	}
	a.flushStatusBuffer()
	reasoning := a.extractReasoning(assistantMessage)
	a.dropTrailingEmptyResponseScaffolding(&s.Messages)
	row := a.buildAssistantMessage(assistantMessage, finishReason)
	row["content"] = "(empty)"
	row["_empty_terminal_sentinel"] = true
	appendLiveMessage(&s.Messages, row)
	if reasoning == "" {
		conversationLoopLog().Warn(fmt.Sprintf("Empty response (no content or reasoning) after %d retries. No fallback available. model=%s provider=%s", a.emptyContentRetries, a.model, a.provider))
		tail := ". No fallback providers configured."
		if len(a.fallbackChain) > 0 {
			tail = " and fallback attempts."
		}
		a.emitDiagnosticStatus("❌ Model returned no content after all retries" + tail)
		return "(empty)"
	}
	conversationLoopLog().Warn(fmt.Sprintf("Reasoning-only response after %d retries; keeping it out of the terminal result (model=%s provider=%s)", a.emptyContentRetries, a.model, a.provider))
	a.emitDiagnosticStatus("⚠️ Model produced reasoning but no visible response after all retries. Returning empty.")
	return "(empty)"
}

// Line written beside a housekeeping only tool round with think blocks stripped, nil without one
func housekeepingReplyLine(a *AIAgent) *string {
	if !pyTruthy(a.lastContentWithTools) || !a.lastContentToolsAllHousekeeping {
		return nil
	}
	line := state.SanitizeSurrogates(pystr.Strip(a.stripThinkBlocks(a.lastContentWithTools)))
	return &line
}

// Consumes the housekeeping reply line as the final response, marking it previewed unless the turn is held
func takeHousekeepingReplyLine(a *AIAgent) *string {
	final := housekeepingReplyLine(a)
	if final == nil {
		return nil
	}
	a.lastContentWithTools, a.lastContentToolsAllHousekeeping = nil, false
	if !a.holdStream {
		a.responseWasPreviewed = true
		text := *final
		a.reusedResponseText = &text
	}
	return final
}

// Decoded reasoning details list of a response message, nil when absent
func reasoningDetailsOf(resp *provider.Response) []any {
	if resp == nil || resp.Message == nil || len(resp.Message.ReasoningDetails) == 0 {
		return nil
	}
	var list []any
	if json.Unmarshal(resp.Message.ReasoningDetails, &list) != nil {
		return nil
	}
	return list
}

// Recovers a final with no visible content through the partial stream, prior content, nudge, prefill, retry and fallback ladder
func recoverEmptyResponse(ctx context.Context, a *AIAgent, s *LoopState, assistantMessage *provider.Response, finishReason, finalResponse string) (PhaseVerdict, error) {
	s.FinalResponse = finalResponse
	if partial := a.currentStreamedAssistantText(); a.hasContentAfterThinkBlock(partial) {
		s.TurnExitReason = "partial_stream_recovery"
		recovered := pystr.Strip(a.stripThinkBlocks(partial))
		conversationLoopLog().Info(fmt.Sprintf("Partial stream content delivered (%d chars) — using as final response", pyLen(recovered)))
		a.emitDiagnosticStatus("↻ Stream interrupted — using delivered content as final response")
		s.FinalResponse = recovered
		a.responseWasPreviewed = false
		return PhaseVerdict{Action: "break"}, nil
	}
	if final := takeHousekeepingReplyLine(a); final != nil {
		s.TurnExitReason = "fallback_prior_turn_content"
		conversationLoopLog().Info("Empty follow-up after tool calls — using prior turn content as final response")
		a.emitDiagnosticStatus("↻ Empty response after tool calls — using earlier content as final answer")
		a.emptyContentRetries = 0
		s.FinalResponse = *final
		return PhaseVerdict{Action: "break"}, nil
	}
	priorWasTool := false
	for _, m := range s.Messages[max(len(s.Messages)-5, 0):] {
		if m != nil && m.Role == "tool" {
			priorWasTool = true
		}
	}
	inlineThinking := inlineThinkRe.MatchString(finalResponse)
	if priorWasTool && !a.postToolEmptyRetried && !inlineThinking {
		a.postToolEmptyRetried = true
		a.lastContentWithTools, a.lastContentToolsAllHousekeeping = nil, false
		conversationLoopLog().Info("Empty response after tool calls — nudging model to continue processing")
		a.BufferDiagnosticStatus("⚠️ Model returned empty after tool calls — nudging to continue")
		nudge := a.buildAssistantMessage(assistantMessage, finishReason)
		nudge["content"] = "(empty)"
		nudge["_empty_recovery_synthetic"] = true
		appendLiveMessage(&s.Messages, nudge)
		appendLiveMessage(&s.Messages, map[string]any{"role": "user", "content": emptyToolResponseNudge, "_empty_recovery_synthetic": true})
		return PhaseVerdict{Action: "continue"}, nil
	}
	msg := assistantMessage.Message
	structured := (msg != nil && (msg.Reasoning != "" || msg.ReasoningContent != "" || len(reasoningDetailsOf(assistantMessage)) > 0)) || inlineThinking
	if structured && a.thinkingPrefillRetries < 2 {
		a.thinkingPrefillRetries++
		conversationLoopLog().Info(fmt.Sprintf("Thinking-only response (no visible content) — prefilling to continue (%d/2)", a.thinkingPrefillRetries))
		a.BufferDiagnosticStatus(fmt.Sprintf("↻ Thinking-only response — prefilling to continue (%d/2)", a.thinkingPrefillRetries))
		interim := a.buildAssistantMessage(assistantMessage, "incomplete")
		interim["_thinking_prefill"] = true
		appendLiveMessage(&s.Messages, interim)
		a.sessionMessages = s.Messages
		return PhaseVerdict{Action: "continue"}, nil
	}
	trulyEmpty := pystr.Strip(a.stripThinkBlocks(finalResponse)) == ""
	candidate := trulyEmpty && (!structured || a.thinkingPrefillRetries >= 2)
	action, interruptResult, deterministic, err := retryEmpty(ctx, a, s, assistantMessage, finishReason, candidate, structured)
	if err != nil {
		return PhaseVerdict{}, err
	}
	if action != "" {
		return PhaseVerdict{Action: action, Result: interruptResult}, nil
	}
	if trulyEmpty && deterministic {
		conversationLoopLog().Warn(fmt.Sprintf("Repeated empty response detected (model=%s provider=%s finish_reason=%s) — skipping remaining retries", a.model, a.provider, finishReason))
		a.BufferDiagnosticStatus("⚠️ Model is repeatedly returning empty content — skipping further retries to avoid repeat charges")
	}
	if trulyEmpty && len(a.fallbackChain) > 0 {
		conversationLoopLog().Warn(fmt.Sprintf("Empty response after %d retries — attempting fallback (model=%s, provider=%s)", a.emptyContentRetries, a.model, a.provider))
		a.BufferDiagnosticStatus("⚠️ Model returning empty responses — switching to fallback provider...")
		if a.tryActivateFallback(ctx, "") {
			s.ActiveSystemPrompt = syncFailoverSystemMessage(a, s.APIMessages, s.ActiveSystemPrompt)
			a.emptyContentRetries = 0
			a.BufferDiagnosticStatus(fmt.Sprintf("↻ Switched to fallback: %s (%s)", a.model, a.provider))
			conversationLoopLog().Info(fmt.Sprintf("Fallback activated after empty responses: now using %s on %s", a.model, a.provider))
			s.PreflightCompressionBlocked = false
			s.APICallCount = refundAPICall(a, s.APICallCount)
			return PhaseVerdict{Action: "continue"}, nil
		}
	}
	s.TurnExitReason = "empty_response_exhausted"
	s.FinalResponse = terminalEmpty(a, s, assistantMessage, finishReason)
	return PhaseVerdict{Action: "break"}, nil
}
