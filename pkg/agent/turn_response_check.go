package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Codex continuation for an output cap stop, content_filter for a filtered stop, stop otherwise
func codexFinishReason(response any) string {
	var status string
	if s, ok := state.ToolCallField(response, "status").(string); ok {
		status = strings.ToLower(pystr.Strip(s))
	}
	var reason *string
	if raw := state.ToolCallField(state.ToolCallField(response, "incomplete_details"), "reason"); raw != nil {
		r := strings.ToLower(pystr.Strip(pyrepr.Str(raw)))
		reason = &r
	}
	if status == "incomplete" && reason != nil && (*reason == "max_output_tokens" || *reason == "length") {
		return "incomplete"
	}
	if status == "incomplete" && reason != nil && *reason == "content_filter" {
		return "content_filter"
	}
	return "stop"
}

// Stop reason mapped to the chat vocabulary, a Bedrock guardrail substitution reading as content_filter
func anthropicResponseFinishReason(t provider.Transport, response any) string {
	message, _ := response.(map[string]any)
	if provider.AnthropicResponseGuardrailIntervened(message) {
		return "content_filter"
	}
	stopReason, _ := message["stop_reason"].(string)
	return t.MapFinishReason(stopReason)
}

// Finish reason per api mode, a cut off Codex function call and a suspicious GLM stop routing to length
func deriveFinishReason(a *AIAgent, response any, messages []*state.Message) (string, error) {
	t := a.getTransport("")
	if t == nil {
		return "", fmt.Errorf("no transport registered for api_mode %q", a.apiMode)
	}
	if a.apiMode == "codex_responses" {
		finishReason := codexFinishReason(response)
		if finishReason == "incomplete" {
			normalized, err := t.NormalizeResponse(response, nil)
			if err != nil {
				return "", err
			}
			if responseHasToolCalls(normalized) {
				return "length", nil
			}
		}
		return finishReason, nil
	}
	if a.apiMode == "anthropic_messages" {
		return anthropicResponseFinishReason(t, response), nil
	}
	normalized, err := t.NormalizeResponse(response, nil)
	if err != nil {
		return "", err
	}
	finishReason := normalized.FinishReason
	if a.apiMode != "bedrock_converse" && a.shouldTreatStopAsTruncated(finishReason, normalized.Message, messagesToRows(messages)) {
		a.vprint(a.logPrefix+"⚠️  Treating suspicious Ollama/GLM stop response as truncated", true, true)
		return "length", nil
	}
	return finishReason, nil
}

// Verifies the response, retrying or failing over an invalid one, routing refusals and truncation, then folds usage
func checkAPIResponse(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	s.APIDuration = recoveryNow() - s.APIStartTime
	s.ThinkingSpinner = stopThinkingSpinner(a, s.ThinkingSpinner)
	if !a.quietMode {
		a.vprint(fmt.Sprintf("%s⏱️  API call completed in %.2fs", a.logPrefix, s.APIDuration), false, false)
	}
	if a.verboseLogging {
		respModel, usage := "N/A", "N/A"
		if pyrepr.Truthy(s.Response) && responseHasField(s.Response, "model") {
			respModel = pyrepr.Str(state.ToolCallField(s.Response, "model"))
		}
		if s.Response != nil && responseHasField(s.Response, "usage") {
			usage = pyrepr.Str(state.ToolCallField(s.Response, "usage"))
		}
		slog.Debug(fmt.Sprintf("API Response received - Model: %s, Usage: %s", respModel, usage))
	}

	invalid, details, err := validateResponseShape(a, s.Response)
	if err != nil {
		return PhaseVerdict{}, err
	}
	if invalid {
		iv, err := retryInvalidResponse(ctx, a, s, details)
		if err != nil {
			return PhaseVerdict{}, err
		}
		if iv.Action != "fallthrough" {
			return iv, nil
		}
	}

	a.turnReceivedProviderResponse = true
	if s.FinishReason, err = deriveFinishReason(a, s.Response, s.Messages); err != nil {
		return PhaseVerdict{}, err
	}
	observability.RecordReplyFinish(ctx, a, s.Response, s.FinishReason)

	if s.FinishReason == "content_filter" {
		rv, err := a.handleContentPolicyRefusal(ctx, RefusalRequest{
			Response: s.Response, Retry: s.Retry, ThinkingSpinner: s.ThinkingSpinner, Messages: s.Messages,
			APIMessages: s.APIMessages, APIKwargs: s.APIKwargs, ActiveSystemPrompt: s.ActiveSystemPrompt,
			ConversationHistory: s.ConversationHistory, APICallCount: s.APICallCount, EffectiveTaskID: s.EffectiveTaskID,
			TurnID: s.TurnID, APIRequestID: s.APIRequestID, APIStartTime: s.APIStartTime, RetryCount: s.RetryCount,
			MaxRetries: s.MaxRetries,
		})
		if err != nil {
			return PhaseVerdict{}, err
		}
		s.ThinkingSpinner = nil
		s.ActiveSystemPrompt = rv.ActiveSystemPrompt
		if rv.Action == "return" {
			return PhaseVerdict{Action: "return", Result: rv.Result}, nil
		}
		s.RetryCount, s.CompressionAttempts = 0, 0
		return PhaseVerdict{Action: "break"}, nil
	}

	if s.FinishReason == "length" {
		tv, err := a.recoverFromTruncation(ctx, TruncationRequest{
			Response: s.Response, FinishReason: s.FinishReason, Retry: s.Retry, Messages: s.Messages,
			ConversationHistory: s.ConversationHistory, APIKwargs: s.APIKwargs, APICallCount: s.APICallCount,
			EffectiveTaskID: s.EffectiveTaskID, CurrentTurnUserIdx: s.CurrentTurnUserIdx,
			LengthContinueRetries: s.LengthContinueRetries, TruncatedResponseParts: s.TruncatedResponseParts,
			TruncatedToolCallRetries: s.TruncatedToolCallRetries, RetryCount: s.RetryCount,
			CompressionAttempts: s.CompressionAttempts,
		})
		if err != nil {
			return PhaseVerdict{}, err
		}
		s.Messages, s.LengthContinueRetries = tv.Messages, tv.LengthContinueRetries
		s.TruncatedResponseParts, s.TruncatedToolCallRetries = tv.TruncatedResponseParts, tv.TruncatedToolCallRetries
		s.RetryCount, s.CompressionAttempts = tv.RetryCount, tv.CompressionAttempts
		switch tv.Action {
		case "return", "break", "continue":
			return PhaseVerdict{Action: tv.Action, Result: tv.Result}, nil
		}
	}

	usage := a.recordResponseUsage(ctx, s.Response, s.Messages, s.APICallCount, s.APIDuration, s.CompressionAttempts, s.MaxCompressionAttempts)
	s.CompressionAttempts = usage.CompressionAttempts
	if usage.Rearmed {
		s.PreflightCompressionBlocked, s.LastPreflightPressure = false, nil
	}

	s.Retry.HasRetried429 = false
	if a.provider == "nous" {
		guard := &provider.NousRateGuard{Home: home.Dir(ctx), Anonymous: provider.IsAnonymousRequest(a.provider, a.apiKey)}
		_ = guard.Clear()
	}
	if err := provider.RelayCompleteLogicalCall(ctx, s.APIRequestID, provider.RelayLogicalCompletion{Outcome: "success"}); err != nil {
		return PhaseVerdict{}, err
	}
	a.touchActivity(fmt.Sprintf("API call #%d completed", s.APICallCount), "", false)
	return PhaseVerdict{Action: "break"}, nil
}

// Malformed or empty response, pool rotation for a Codex soft failure, eager fallback, terminal result or interruptible backoff
func retryInvalidResponse(ctx context.Context, a *AIAgent, s *LoopState, errorDetails []string) (PhaseVerdict, error) {
	errorMessage := strings.Join(errorDetails, ", ")
	if errorMessage == "" {
		errorMessage = "Invalid API response"
	}
	a.invokeAPIRequestErrorHook(ctx, map[string]any{
		"task_id": s.EffectiveTaskID, "turn_id": s.TurnID, "api_request_id": s.APIRequestID,
		"api_call_count": s.APICallCount, "api_start_time": s.APIStartTime, "api_kwargs": s.APIKwargs,
		"error_type": "InvalidAPIResponse", "error_message": errorMessage,
		"status_code": pyAttr(state.ToolCallField(s.Response, "error"), "code"),
		"retry_count": s.RetryCount, "max_retries": s.MaxRetries, "retryable": true, "reason": "invalid_response",
	})
	s.ThinkingSpinner = stopThinkingSpinner(a, s.ThinkingSpinner)

	soft, softCtx := classifyCodexSoftFailure(a, s.Response)
	if soft != nil && (soft.Reason == provider.ReasonRateLimit || soft.Reason == provider.ReasonBilling || soft.IsAuth()) {
		var recovered bool
		recovered, s.Retry.HasRetried429 = a.recoverWithCredentialPool(ctx, nil, s.Retry.HasRetried429, soft.Reason, softCtx, soft.BillingUnverified())
		if recovered {
			a.BufferDiagnosticStatus(fmt.Sprintf("🔄 Codex soft failure (%s) — switched to the next pool credential, retrying...", soft.Reason))
			return PhaseVerdict{Action: "continue"}, nil
		}
	}
	s.RetryCount++

	if a.fallbackIndex < len(a.fallbackChain) {
		a.BufferDiagnosticStatus("⚠️ Empty/malformed response — switching to fallback...")
	}
	if a.tryActivateFallback(ctx, "") {
		s.ActiveSystemPrompt = armFallbackRestart(ctx, a, s.APIMessages, s.ActiveSystemPrompt, s.Retry)
		s.RetryCount, s.CompressionAttempts = 0, 0
		return PhaseVerdict{Action: "break"}, nil
	}

	errorMsg, providerName, failureHint := describeInvalidResponse(a, s.Response, s.APIDuration)
	blines(a,
		fmt.Sprintf("⚠️  Invalid API response (attempt %d/%d): %s", s.RetryCount, s.MaxRetries, strings.Join(errorDetails, ", ")),
		"   🏢 Provider: "+providerName,
		"   📝 Provider message: "+provider.CleanErrorMessage(errorMsg),
		"   ⏱️  "+failureHint)

	if s.RetryCount >= s.MaxRetries {
		if a.hasPendingFallback() {
			a.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Max retries (%d) for invalid responses — trying fallback...", s.MaxRetries))
		}
		if a.tryActivateFallback(ctx, "") {
			s.ActiveSystemPrompt = armFallbackRestart(ctx, a, s.APIMessages, s.ActiveSystemPrompt, s.Retry)
			s.RetryCount, s.CompressionAttempts = 0, 0
			return PhaseVerdict{Action: "break"}, nil
		}
		a.flushStatusBuffer()
		a.emitDiagnosticStatus(fmt.Sprintf("❌ Max retries (%d) exceeded for invalid responses. Giving up.", s.MaxRetries))
		conversationLoopLog().Error(fmt.Sprintf("%sInvalid API response after %d retries.", a.logPrefix, s.MaxRetries))
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		label := providerName
		if providerName == "Unknown" || providerName == "" || strings.HasPrefix(providerName, "model=") {
			label = ProviderLabelFor(a.provider)
		}
		finalResponse, err := tools.SiteCopy(ctx, "invalid_response", map[string]any{
			"label": label, "attempts": s.MaxRetries, "detail": failureHint,
		})
		if err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "return", Result: StampFailure(map[string]any{
			"final_response": finalResponse,
			"messages":       s.Messages,
			"completed":      false,
			"api_calls":      s.APICallCount,
			"error":          fmt.Sprintf("Invalid API response after %d retries: %s", s.MaxRetries, failureHint),
			"failed":         true,
		}, InvalidResponseFailureReason(s.Response), true)}, nil
	}

	wait := provider.JitteredBackoff(s.RetryCount, provider.BackoffOptions{BaseDelay: 5 * time.Second, MaxDelay: 120 * time.Second})
	waitTime := wait.Seconds()
	a.bufferVprint(fmt.Sprintf("⏳ Retrying in %.1fs (%s)...", waitTime, failureHint))
	conversationLoopLog().Warn(fmt.Sprintf("Invalid API response (retry %d/%d): %s | Provider: %s", s.RetryCount, s.MaxRetries, strings.Join(errorDetails, ", "), providerName))

	interrupted, err := interruptibleBackoffSleep(ctx, a, waitTime, s.Retry, &s.Messages, s.ConversationHistory, s.APICallCount,
		"Interrupt detected during retry wait, aborting.",
		fmt.Sprintf("Operation interrupted during retry (%s, attempt %d/%d).", failureHint, s.RetryCount, s.MaxRetries),
		fmt.Sprintf("retry backoff (%d/%d)", s.RetryCount, s.MaxRetries))
	if err != nil {
		return PhaseVerdict{}, err
	}
	if interrupted != nil {
		return PhaseVerdict{Action: "return", Result: interrupted}, nil
	}
	if s.Retry.RestartWithRedirectedMessages {
		return PhaseVerdict{Action: "break"}, nil
	}
	return PhaseVerdict{Action: "continue"}, nil
}
