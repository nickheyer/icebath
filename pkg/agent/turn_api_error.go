package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The locals one API error threads from classification through routing, overflow recovery and settlement
type apiErrorTurn struct {
	apiError               error
	classified             *provider.Classified
	statusCode             *int
	errorContext           map[string]any
	errorType              string
	errorMsg               string
	providerName           string
	baseURL                string
	model                  string
	recoveredWithPool      bool
	isRateLimited          bool
	isZAICodingOverload    bool
	isContextLengthError   bool
	wrappedOutputCapBudget *int
}

// Response headers an API error carries, nil when it has none
func apiErrorHeaders(err error) http.Header {
	var hc provider.HeaderCarrier
	if errors.As(err, &hc) {
		return hc.ResponseHeaders()
	}
	return nil
}

// Captures the refused Nous usage window credits headers, any failure logged and swallowed
func captureRefusedCredits(a *AIAgent, status int, apiError error) {
	defer func() {
		if r := recover(); r != nil {
			conversationLoopLog().Debug(fmt.Sprintf("credits ▸ header capture on %d failed (fail-open)", status), "error", fmt.Sprint(r))
		}
	}()
	headers := apiErrorHeaders(apiError)
	if provider.ResponseIsUsageWindow(headers) {
		a.creditsCapture().CaptureCredits(headers, true)
	}
}

// Recovers from an API error in hermes order, every fallback activation leaving the retry loop with the rebuild armed
func handleAPIError(ctx context.Context, a *AIAgent, s *LoopState, apiError error) (PhaseVerdict, error) {
	s.ThinkingSpinner = stopThinkingSpinner(a, s.ThinkingSpinner)

	recovered, err := recoverBeforeClassification(ctx, a, s, apiError)
	if err != nil || recovered {
		return PhaseVerdict{Action: "continue"}, err
	}

	e := &apiErrorTurn{apiError: apiError, errorContext: provider.ExtractAPIErrorContext(apiError)}
	if status, ok := surfaceExceptionStatus(apiError); ok {
		e.statusCode = &status
	}
	if e.statusCode != nil && (*e.statusCode == 402 || *e.statusCode == 403) && a.provider == "nous" {
		captureRefusedCredits(a, *e.statusCode, apiError)
	}

	if tools.InterpreterShuttingDown(apiError) {
		conversationLoopLog().Warn(fmt.Sprintf("%sInterpreter is shutting down — abandoning turn during API call #%d (%s)", a.logPrefix, s.APICallCount, apiError))
		summary := "Turn abandoned: the process was shutting down before the model call could complete."
		return PhaseVerdict{Action: "return", Result: map[string]any{
			"final_response": summary, "messages": s.Messages, "api_calls": s.APICallCount,
			"completed": false, "failed": true, "error": summary,
			"failure_reason": "interpreter_shutdown", "failure_retryable": false,
		}}, nil
	}

	ctxLen := 200000
	if a.contextCompressor != nil {
		ctxLen = a.contextCompressor.Base().ContextLength
	}
	in := provider.ClassifyInput{Err: apiError, Headers: apiErrorHeaders(apiError), Provider: a.provider, Model: a.model,
		ApproxTokens: s.ApproxTokens, ContextLength: ctxLen, NumMessages: len(s.APIMessages), BaseURL: a.baseURL, APIKey: a.apiKey}
	if e.statusCode != nil {
		in.Status = *e.statusCode
	}
	var apiErr *provider.APIError
	if errors.As(apiError, &apiErr) {
		in.Body = apiErr.Body
	}
	cl := provider.ClassifyError(in)
	e.classified = cl
	conversationLoopLog().Debug(fmt.Sprintf("Error classified: reason=%s status=%s retryable=%s compress=%s rotate=%s fallback=%s",
		cl.Reason, pyIntOrNone(cl.StatusCode), pyBool(cl.Retryable), pyBool(cl.ShouldCompress), pyBool(cl.ShouldRotateCredential), pyBool(cl.ShouldFallback)))
	var statusArg any
	if e.statusCode != nil {
		statusArg = *e.statusCode
	}
	a.invokeAPIRequestErrorHook(ctx, map[string]any{
		"task_id": s.EffectiveTaskID, "turn_id": s.TurnID, "api_request_id": s.APIRequestID,
		"api_call_count": s.APICallCount, "api_start_time": s.APIStartTime, "api_kwargs": s.APIKwargs,
		"error_type": surfaceExceptionName(apiError), "error_message": apiError.Error(), "status_code": statusArg,
		"retry_count": s.RetryCount, "max_retries": s.MaxRetries, "retryable": cl.Retryable,
		"reason": string(cl.Reason),
	})

	recovered, e.recoveredWithPool, err = recoverAfterClassification(ctx, a, s, e)
	if err != nil || recovered {
		return PhaseVerdict{Action: "continue"}, err
	}

	s.RetryCount++
	elapsed := apiErrorWallNow() - s.APIStartTime
	a.TouchActivity(fmt.Sprintf("API error recovery (attempt %d/%d)", s.RetryCount, s.MaxRetries))
	e.errorType, e.errorMsg, e.providerName, e.baseURL, e.model = logAPIErrorAttempt(ctx, a, s, e, elapsed, cl.Retryable)

	if a.Interrupted() {
		if a.clearInterrupt(true) {
			s.Retry.RestartWithRedirectedMessages = true
			return PhaseVerdict{Action: "break"}, nil
		}
		result, err := abortTurnOnInterrupt(a, &s.Messages, s.ConversationHistory, s.APICallCount,
			"Interrupt detected during error handling, aborting retries.",
			fmt.Sprintf("Operation interrupted: handling API error (%s: %s).", e.errorType, provider.CleanErrorMessage(apiError.Error())))
		return PhaseVerdict{Action: "return", Result: result}, err
	}

	v, err := routeClassifiedError(ctx, a, s, e)
	if err != nil || v.Action != "fallthrough" {
		return v, err
	}
	v, err = recoverFromOverflow(ctx, a, s, e)
	if err != nil || v.Action != "fallthrough" {
		return v, err
	}
	return settleUnrecoveredError(ctx, a, s, e)
}

// Wall clock seconds for the elapsed time of a failed attempt, replaced by tests
var apiErrorWallNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Python repr of an optional status code
func pyIntOrNone(n int) string {
	if n == 0 {
		return "None"
	}
	return fmt.Sprint(n)
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// ValueError and TypeError shaped local bugs, excluding encode, decode, TLS, stream parse and NoneType iteration failures
func isLocalValidationError(apiError error) bool { return provider.IsLocalValidationError(apiError) }

// Settles an error every recovery declined into a client error exit, retry exhaustion, or the interruptible backoff
func settleUnrecoveredError(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn) (PhaseVerdict, error) {
	cl, retry := e.classified, s.Retry
	localValidation := isLocalValidationError(e.apiError)
	shrinkSpent := cl.Reason == provider.ReasonImageTooLarge && retry.ImageShrinkRetryAttempted
	reasoningSpent := cl.Reason == provider.ReasonReasoningMandatory && retry.ReasoningMandatoryRetryAttempted
	isClientError := (localValidation || shrinkSpent || reasoningSpent ||
		(!cl.Retryable && !cl.ShouldCompress && !provider.RetryableClientReasons[cl.Reason])) && !e.isContextLengthError

	if isClientError {
		markEntitlementRejectedModel(a, e.apiError)
		if !retry.ProviderStaleRefreshAttempted {
			if profile := credentialRefreshProfile(ctx, a); profile != nil {
				var status any
				if e.statusCode != nil {
					status = *e.statusCode
				}
				message := e.apiError.Error()
				var apiErr *provider.APIError
				if errors.As(e.apiError, &apiErr) && apiErr.Message != "" {
					message = apiErr.Message
				}
				stale := provider.AskOnErrorPath(ctx, profile, "is_stale_credential_error", false, map[string]any{"status_code": status, "message": message})
				if pyTruthy(stale) {
					retry.ProviderStaleRefreshAttempted = true
					refreshed, err := a.tryRefreshProviderRuntimeCredentials(ctx, "stale")
					if err != nil {
						return PhaseVerdict{}, err
					}
					if refreshed {
						notice, _ := provider.AskOnErrorPath(ctx, profile, "credential_refresh_notice", "", map[string]any{"reason": "stale"}).(string)
						if notice == "" {
							name := profile.DisplayName
							if name == "" {
								name = profile.Name
							}
							notice = name + " credential renewed after a stale-credential error. Retrying request..."
						}
						a.bufferVprint("🔐 " + notice)
						s.RetryCount = 0
						return PhaseVerdict{Action: "continue"}, nil
					}
				}
			}
		}
		unclassifiedLocal := localValidation && cl.Reason == provider.ReasonUnknown
		if cl.ShouldFallback || unclassifiedLocal || shrinkSpent || reasoningSpent {
			if a.hasPendingFallback() {
				label, ok := provider.NonretryableLabel(cl.Reason)
				if !ok {
					label = fmt.Sprintf("Non-retryable error (HTTP %s)", pyStatus(e.statusCode))
				}
				a.BufferDiagnosticStatus(fmt.Sprintf("⚠️ %s — trying fallback...", label))
			}
			if a.tryActivateFallbackAt(ctx, cl.Reason, e.errorContext["reset_at"]) {
				s.ActiveSystemPrompt = armFallbackRestart(ctx, a, s.APIMessages, s.ActiveSystemPrompt, retry)
				s.RetryCount, s.CompressionAttempts = 0, 0
				return PhaseVerdict{Action: "break"}, nil
			}
		}
		delivered := settleDeliveredPartial(ctx, a, &s.Messages, s.CurrentTurnUserIdx)
		result, err := settleNonretryableResult(ctx, a, s, e, delivered)
		return PhaseVerdict{Action: "return", Result: result}, err
	}

	attemptsMade := min(s.RetryCount, s.MaxRetries)
	if e.isRateLimited && s.RetryCount < s.MaxRetries && freeTierCooldownEndsTurn(a, e.apiError, e.baseURL) {
		conversationLoopLog().Info(fmt.Sprintf("%sFree-tier cooldown outlasts the attended wait — ending retries after attempt %d", a.logPrefix, s.RetryCount))
		s.RetryCount = s.MaxRetries
	}
	if s.RetryCount >= s.MaxRetries {
		if !retry.PrimaryRecoveryAttempted && a.tryRecoverPrimaryTransport(ctx, e.apiError, s.RetryCount) {
			retry.PrimaryRecoveryAttempted = true
			s.RetryCount = 0
			retry.HasRetried429 = false
			a.fallbackIndex = 0
			a.fallbackActivated = false
			return PhaseVerdict{Action: "continue"}, nil
		}
		if a.hasPendingFallback() {
			a.BufferDiagnosticStatus(fmt.Sprintf("⚠️ Max retries (%d) exhausted — trying fallback...", s.MaxRetries))
		}
		if a.tryActivateFallbackAt(ctx, cl.Reason, e.errorContext["reset_at"]) {
			s.ActiveSystemPrompt = armFallbackRestart(ctx, a, s.APIMessages, s.ActiveSystemPrompt, retry)
			s.RetryCount, s.CompressionAttempts = 0, 0
			return PhaseVerdict{Action: "break"}, nil
		}
		ladder, err := autoRecoverAfterExhaustion(ctx, a, s, e)
		if err != nil {
			return PhaseVerdict{}, err
		}
		if ladder != nil {
			if ladder.Action == "continue" {
				s.RetryCount = 0
			}
			return *ladder, nil
		}
		delivered := settleDeliveredPartial(ctx, a, &s.Messages, s.CurrentTurnUserIdx)
		result, err := settleExhaustedResult(ctx, a, s, e, attemptsMade, delivered)
		return PhaseVerdict{Action: "return", Result: result}, err
	}

	waitTime := computeErrorBackoff(a, e.apiError, s.RetryCount, s.MaxRetries, e.isRateLimited, e.isZAICodingOverload, e.baseURL, e.model)
	interrupted, err := interruptibleBackoffSleep(ctx, a, waitTime, retry, &s.Messages, s.ConversationHistory, s.APICallCount,
		"Interrupt detected during retry wait, aborting.",
		fmt.Sprintf("Operation interrupted: retrying API call after error (retry %d/%d).", s.RetryCount, s.MaxRetries),
		fmt.Sprintf("error retry backoff (%d/%d)", s.RetryCount, s.MaxRetries))
	if err != nil {
		return PhaseVerdict{}, err
	}
	if interrupted != nil {
		return PhaseVerdict{Action: "return", Result: interrupted}, nil
	}
	if retry.RestartWithRedirectedMessages {
		return PhaseVerdict{Action: "break"}, nil
	}
	return PhaseVerdict{Action: "fallthrough"}, nil
}

// Terminal result builders the settle path ends a turn with, replaced by tests
var (
	settleNonretryableResult = nonretryableClientErrorResult
	settleExhaustedResult    = maxRetriesExhaustedResult
)

// Python str of an optional status code
func pyStatus(code *int) string {
	if code == nil {
		return "None"
	}
	return fmt.Sprint(*code)
}
