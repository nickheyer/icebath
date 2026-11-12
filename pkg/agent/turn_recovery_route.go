package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Reasons the overflow recovery paths handle
var overflowReasons = map[provider.FailoverReason]bool{
	provider.ReasonLongContextTier: true, provider.ReasonPayloadTooLarge: true, provider.ReasonContextOverflow: true,
}

// Reasons that switch to the fallback chain immediately
var eagerRateLimitReasons = map[provider.FailoverReason]bool{
	provider.ReasonRateLimit: true, provider.ReasonBilling: true, provider.ReasonUpstreamRateLimit: true,
}

// Reasons that switch to the fallback chain after one retry
var transportFailureReasons = map[provider.FailoverReason]bool{provider.ReasonTimeout: true, provider.ReasonOverloaded: true}

// Context window the Anthropic long context tier allows without extra usage
const longContextTierCap = 200000

// Caps the compressor's context window at the long context tier limit, returning the previous length
func capLongContextTier(a *AIAgent) int {
	compressor := a.contextCompressor
	oldCtx := compressor.Base().ContextLength
	if oldCtx > longContextTierCap {
		compressor.UpdateModel(a.model, longContextTierCap, a.baseURL, a.apiKey, a.provider, a.apiMode)
		if c, ok := compressor.(*ContextCompressor); ok && c != nil {
			c.contextProbed = true
			c.contextProbePersistable = false
		}
		a.bufferVprint(fmt.Sprintf("⚠️  Anthropic long-context tier requires extra usage — reducing context: %s → %s tokens",
			pystr.GroupThousands(int64(oldCtx)), pystr.GroupThousands(longContextTierCap)))
	}
	return oldCtx
}

// Status line announcing an eager fallback switch
func eagerFallbackStatus(classified *provider.Classified, isUpstream, isTransportFailure bool) string {
	if isUpstream {
		name, ok := classified.Context["upstream_provider"]
		if !ok {
			name = "aggregator"
		}
		return fmt.Sprintf("⚠️ Upstream %v rate-limited — switching to fallback model...", name)
	}
	if classified.Reason == provider.ReasonBilling {
		if classified.BillingUnverified() {
			return "⚠️ Provider reported usage/credit exhaustion " +
				"(unverified — may be a content-filter rejection) " +
				"— switching to fallback provider..."
		}
		return "⚠️ Billing or credits exhausted — switching to fallback provider..."
	}
	if isTransportFailure {
		return "⚠️ Provider unreachable — switching to fallback provider..."
	}
	return "⚠️ Rate limited — switching to fallback provider..."
}

// Records a genuine account level Nous 429 to the cross session breaker, leaving upstream capacity 429s alone
func isGenuineNousRateLimit(ctx context.Context, a *AIAgent, apiError error, errorContext map[string]any, classified *provider.Classified) (genuine bool) {
	defer func() {
		if r := recover(); r != nil {
			genuine = false
		}
	}()
	headers := apiErrorHeaders(apiError)
	anonymous := provider.IsAnonymousRequest(a.provider, a.apiKey)
	classifiedCtx := classified.Context
	genuine = (anonymous && provider.IsLongWelcomeRateLimit(classifiedCtx)) ||
		provider.IsGenuineNousRateLimit(headers, a.creditsCapture().RateLimit(), time.Now())
	if !genuine {
		conversationLoopLog().Info("Nous 429 looks like upstream capacity (no exhausted bucket in headers or last-known state) -- not tripping cross-session breaker.")
		return false
	}
	merged := maps.Clone(errorContext)
	if merged == nil {
		merged = map[string]any{}
	}
	maps.Copy(merged, classifiedCtx)
	guard := &provider.NousRateGuard{Home: home.Dir(ctx), Anonymous: anonymous}
	if err := guard.Record(headers, provider.ContextResetAt(merged), 300*time.Second); err != nil {
		provider.LogNousRateLimitWriteFailure(err)
	}
	return true
}

// Tool schemas of the agent as maps, nil when it has none
func agentToolMaps(a *AIAgent) []map[string]any {
	if len(a.tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(a.tools))
	for _, t := range a.tools {
		if d, ok := t.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// Ordered recoveries between classification and overflow handling, fallthrough to proceed
func routeClassifiedError(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn) (PhaseVerdict, error) {
	classified, apiError, retry := e.classified, e.apiError, s.Retry
	e.isRateLimited, e.wrappedOutputCapBudget, e.isZAICodingOverload = false, nil, false
	fallbackBreak := func() (PhaseVerdict, error) {
		s.ActiveSystemPrompt = armFallbackRestart(ctx, a, s.APIMessages, s.ActiveSystemPrompt, retry)
		s.RetryCount, s.CompressionAttempts = 0, 0
		return PhaseVerdict{Action: "break"}, nil
	}

	_, parsedCap := provider.ParseAvailableOutputTokensFromError(e.errorMsg)
	isOutputCapError := provider.IsOutputCapError(e.errorMsg) || parsedCap
	if overflowReasons[classified.Reason] && !a.compressionEnabled && !isOutputCapError {
		a.flushStatusBuffer()
		vlines(a,
			"❌ The conversation is too long for the model and automatic shrinking is off (compression.enabled: false).",
			"   💡 Run /compress to shrink it now, /new to start fresh, "+
				"pick a model with a bigger context window, or remove attachments.")
		conversationLoopLog().Error(fmt.Sprintf("%sContext overflow (%s) with auto-compaction disabled — not compressing.", a.logPrefix, classified.Reason))
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		finalResponse, err := tools.SiteCopy(ctx, "compression_disabled", map[string]any{"model": a.model})
		if err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "return", Result: StampFailure(map[string]any{
			"final_response": finalResponse, "messages": s.Messages, "completed": false,
			"api_calls": s.APICallCount, "error": finalResponse, "partial": true, "failed": true,
			"compaction_disabled": true,
		}, "context_overflow", false)}, nil
	}

	if classified.Reason == provider.ReasonLongContextTier {
		oldCtx := capLongContextTier(a)
		s.CompressionAttempts++
		if s.CompressionAttempts <= s.MaxCompressionAttempts {
			originalLen := len(s.Messages)
			approx := provider.EstimateRequestTokens(ctx, apiRowsToMessages(s.APIMessages), "", agentToolMaps(a), true)
			trigger, reason := "overflow", string(classified.Reason)
			compressed, prompt, err := a.compressContext(ctx, messagesToRows(s.Messages), s.SystemMessage, compressContextOptions{
				ApproxTokens: &approx, TaskID: s.EffectiveTaskID, Trigger: &trigger, OverflowReason: &reason,
			})
			if err != nil {
				return PhaseVerdict{}, err
			}
			s.Messages, s.ActiveSystemPrompt = rowsToMessages(compressed), prompt
			s.ConversationHistory = rowsToMessages(conversationHistoryAfterCompression(a, compressed, messagesToRows(s.ConversationHistory)))
			if len(s.Messages) < originalLen || oldCtx > longContextTierCap {
				a.BufferDiagnosticStatus(CompressionRetryContextReducedStatus(longContextTierCap, oldCtx))
				recoverySleep(2 * time.Second)
				s.ProviderOverflowRecoveryPending = true
				retry.RestartWithCompressedMessages = true
				return PhaseVerdict{Action: "break"}, nil
			}
		}
	}

	e.isRateLimited = eagerRateLimitReasons[classified.Reason]
	if classified.Reason == provider.ReasonRateLimit {
		if budget, ok := provider.ParseAvailableOutputTokensFromError(e.errorMsg); ok {
			e.wrappedOutputCapBudget = &budget
		}
	}
	isTransportFailure := transportFailureReasons[classified.Reason]
	var apiErr *provider.APIError
	errors.As(apiError, &apiErr)
	e.isZAICodingOverload = apiErr != nil && provider.IsZAICodingOverload(e.baseURL, e.model, apiErr)
	if e.isZAICodingOverload {
		s.MaxRetries = max(s.MaxRetries, provider.ZAICodingOverloadRetryCeiling())
	}
	shouldFallback := ((e.isRateLimited && e.wrappedOutputCapBudget == nil) || (isTransportFailure && s.RetryCount >= 2)) &&
		!pyTruthy(classified.Context["usage_window"])
	if shouldFallback && a.fallbackIndex < len(a.fallbackChain) {
		isUpstream := classified.Reason == provider.ReasonUpstreamRateLimit
		poolMayRecover := !isUpstream && poolMayRecoverFromRateLimit(a.credentialPool)
		if !poolMayRecover {
			a.BufferDiagnosticStatus(eagerFallbackStatus(classified, isUpstream, isTransportFailure))
			if a.tryActivateFallbackAt(ctx, classified.Reason, e.errorContext["reset_at"]) {
				return fallbackBreak()
			}
		}
	}

	if classified.IsAuth() && !retry.AuthFailoverAttempted && a.fallbackIndex < len(a.fallbackChain) {
		retry.AuthFailoverAttempted = true
		a.BufferDiagnosticStatus("🔐 Authentication failed and could not be refreshed — " +
			"switching to fallback provider...")
		if a.tryActivateFallback(ctx, classified.Reason) {
			return fallbackBreak()
		}
	}

	if e.isRateLimited && a.provider == "nous" && classified.Reason == provider.ReasonRateLimit && !e.recoveredWithPool &&
		isGenuineNousRateLimit(ctx, a, apiError, e.errorContext, classified) {
		s.RetryCount = max(0, s.MaxRetries-1)
		return PhaseVerdict{Action: "continue"}, nil
	}
	return PhaseVerdict{Action: "fallthrough"}, nil
}
