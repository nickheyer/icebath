package agent

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const overflowRetryHint = "   💡 Try /new to start a fresh conversation, or /compress to retry compression."

// Share of the known window below which a provider context rejection is not explained by the transcript
const unexplainedRejectionFraction = 0.5

var (
	minimaxAnthropicURLs = []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"}
	quotedTokenCountRe   = regexp.MustCompile(`\p{Nd}{4,}`)
)

// Compression pass and rough estimators the overflow handlers call, replaced by tests
var (
	overflowCompressContext  = (*AIAgent).compressContext
	overflowEstimateRequest  = estimateRequestRowsRough
	overflowEstimateMessages = func(ctx context.Context, msgs []*state.Message) int {
		return provider.EstimateMessagesTokens(msgs, provider.CurrentImageTokenCost(ctx), true)
	}
)

// Working state the overflow handlers share, the loop locals rebound in place on the loop state
type overflowRecovery struct {
	ctx            context.Context
	a              *AIAgent
	s              *LoopState
	retry          *TurnRetryState
	overflowReason *string
}

// How a failed overflow turn ends, compression exhausted unless the exit is soft
type overflowFailure struct {
	notices      []string
	log          string
	softExit     bool
	reason       string
	retryable    bool
	extraResults map[string]any
}

func (r *overflowRecovery) done(action string, result map[string]any) PhaseVerdict {
	return PhaseVerdict{Action: action, Result: result}
}

// Ends the turn failed and partial, the buffered retry trace flushed first when notices are given
func (r *overflowRecovery) failTurn(finalResponse string, f overflowFailure) (PhaseVerdict, error) {
	a := r.a
	if len(f.notices) > 0 {
		a.flushStatusBuffer()
		for _, line := range f.notices {
			a.vprint(a.logPrefix+line, true, true)
		}
	}
	if f.log != "" {
		conversationLoopLog().Error(f.log)
	}
	if err := a.persistSession(&r.s.Messages, r.s.ConversationHistory); err != nil {
		return PhaseVerdict{}, err
	}
	reason := f.reason
	if reason == "" {
		reason = "context_overflow"
	}
	result := StampFailure(map[string]any{
		"final_response": finalResponse, "messages": r.s.Messages, "completed": false,
		"api_calls": r.s.APICallCount, "error": finalResponse, "partial": true, "failed": true,
	}, reason, f.retryable)
	if !f.softExit {
		result["compression_exhausted"] = true
	}
	maps.Copy(result, f.extraResults)
	return r.done("return", result), nil
}

// Site copy failure for a code that names the model, the error when the copy cannot render
func (r *overflowRecovery) failWithCopy(code string, fields map[string]any, f overflowFailure) (PhaseVerdict, error) {
	text, err := tools.SiteCopy(r.ctx, code, fields)
	if err != nil {
		return PhaseVerdict{}, err
	}
	return r.failTurn(text, f)
}

// Bumps the compression attempts, the terminal verdict once the cap is exceeded
func (r *overflowRecovery) countAttempt(payloadTooLarge bool) (*PhaseVerdict, error) {
	r.s.CompressionAttempts++
	limit := r.s.MaxCompressionAttempts
	if r.s.CompressionAttempts <= limit {
		return nil, nil
	}
	a := r.a
	var v PhaseVerdict
	var err error
	if payloadTooLarge {
		v, err = r.failWithCopy("payload_too_large", map[string]any{"model": a.model}, overflowFailure{
			notices: []string{fmt.Sprintf("❌ Max compression attempts (%d) reached for payload-too-large error.", limit), overflowRetryHint},
			log:     fmt.Sprintf("%s413 compression failed after %d attempts.", a.logPrefix, limit),
		})
	} else {
		v, err = r.failWithCopy("context_overflow", map[string]any{"model": a.model}, overflowFailure{
			notices: []string{fmt.Sprintf("❌ Max compression attempts (%d) reached.", limit), overflowRetryHint},
			log:     fmt.Sprintf("%sContext compression failed after %d attempts.", a.logPrefix, limit),
		})
	}
	return &v, err
}

// One cooldown bypassing compression pass, a soft deferral when a lock or transient block no oped it
func (r *overflowRecovery) compress(requestTokens int, failOnTimeout bool) (*PhaseVerdict, error) {
	a, s := r.a, r.s
	rows := messagesToRows(s.Messages)
	tokens, trigger := requestTokens, "overflow"
	out, system, err := overflowCompressContext(a, r.ctx, rows, s.SystemMessage, compressContextOptions{
		ApproxTokens: &tokens, TaskID: s.EffectiveTaskID, BypassCooldown: true, Trigger: &trigger, OverflowReason: r.overflowReason})
	if err != nil {
		return nil, err
	}
	lc := liveCompaction{messages: s.Messages, rows: rows, system: system, skipped: true}
	if !sameRowSlice(out, rows) {
		lc = liveCompaction{messages: rowsToMessages(out), rows: out, system: system}
	}
	s.Messages, s.ActiveSystemPrompt = lc.messages, lc.system
	if lc.skipped {
		var deferred map[string]any
		if compressionSkippedDueToLock(a) {
			deferred = compressionDeferredResult(a, s.Messages, s.APICallCount, "lock")
		} else if compressionBlockedTransiently(a) {
			deferred = compressionDeferredResult(a, s.Messages, s.APICallCount, "transient_block")
		}
		if deferred != nil {
			s.CompressionAttempts--
			if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
				return nil, err
			}
			v := r.done("return", deferred)
			return &v, nil
		}
	}
	if failOnTimeout && contextCompressionTimedOut(a) {
		v, err := r.failTurn(compressionTimeoutFinalResponse, overflowFailure{extraResults: map[string]any{"turn_exit_reason": "context_compression_timeout"}})
		return &v, err
	}
	s.ConversationHistory = historyAfterCompaction(a, lc, s.ConversationHistory)
	return nil, nil
}

// Compression scored in message count and re estimated tokens, with whether it shrank and the new tokens
func (r *overflowRecovery) compressScoredByTokens(requestTokens int, failOnTimeout bool) (*PhaseVerdict, bool, int, error) {
	originalLen := len(r.s.Messages)
	originalTokens := overflowEstimateMessages(r.ctx, r.s.Messages)
	deferred, err := r.compress(requestTokens, failOnTimeout)
	if err != nil || deferred != nil {
		return deferred, false, originalTokens, err
	}
	newLen := len(r.s.Messages)
	newTokens := overflowEstimateMessages(r.ctx, r.s.Messages)
	shrankTokens := newTokens > 0 && float64(newTokens) < float64(originalTokens)*0.95
	if newLen < originalLen {
		r.a.BufferDiagnosticStatus(CompressionRetryMessagesStatus(originalLen, newLen))
	} else if shrankTokens {
		r.a.BufferDiagnosticStatus(CompressionRetryTokensStatus(originalTokens, newTokens))
	}
	return nil, newLen < originalLen || shrankTokens, newTokens, nil
}

// Overhead aware request size of the api messages plus the tool schemas
func (r *overflowRecovery) requestTokens() int {
	return overflowEstimateRequest(r.ctx, toolDefsOf(r.s.APIMessages), "", toolDefsOf(r.a.tools))
}

// Payload too large recovery scoring compression progress in serialized payload bytes, never tokens
func recoverPayloadTooLarge(r *overflowRecovery) (PhaseVerdict, error) {
	a, s := r.a, r.s
	if exhausted, err := r.countAttempt(true); err != nil || exhausted != nil {
		return derefVerdict(exhausted), err
	}
	a.BufferDiagnosticStatus(fmt.Sprintf("⚠️  Request payload too large (413) — compression attempt %d/%d...", s.CompressionAttempts, s.MaxCompressionAttempts))
	originalLen := len(s.Messages)
	originalBytes := serializedMessagesBytes(anyRows(messagesToRows(s.Messages)))
	if deferred, err := r.compress(r.requestTokens(), false); err != nil || deferred != nil {
		return derefVerdict(deferred), err
	}
	s.ApproxTokens = overflowEstimateMessages(r.ctx, s.Messages)
	newBytes := serializedMessagesBytes(anyRows(messagesToRows(s.Messages)))
	if len(s.Messages) < originalLen || (newBytes > 0 && float64(newBytes) < float64(originalBytes)*0.95) {
		if len(s.Messages) < originalLen {
			a.BufferDiagnosticStatus(CompressionRetryMessagesStatus(originalLen, len(s.Messages)))
		} else {
			a.BufferDiagnosticStatus(fmt.Sprintf("🗜️ Compressed %s → %s payload bytes, retrying...", groupThousands(originalBytes), groupThousands(newBytes)))
		}
		recoverySleep(2 * time.Second)
		r.retry.RestartWithCompressedMessages = true
		return r.done("break", nil), nil
	}
	if a.tryStripImagePartsFromToolMessages(s.APIMessages, false) {
		a.BufferDiagnosticStatus("📐 Compression could not reduce the request further — removed retained vision payloads and retrying...")
		return r.done("continue", nil), nil
	}
	return r.failWithCopy("payload_too_large", map[string]any{"model": a.model}, overflowFailure{
		notices: []string{"❌ Payload too large and cannot compress further.", overflowRetryHint},
		log:     fmt.Sprintf("%s413 payload too large. Cannot compress further.", a.logPrefix),
	})
}

// Clamps the output cap to the provider budget and compresses so the retry does not spin on max tokens alone
func clampOutputCap(r *overflowRecovery, availableOut, oldCtx int) (PhaseVerdict, error) {
	a := r.a
	requestInputEstimate := r.requestTokens()
	localAvailableOut := oldCtx - requestInputEstimate
	safeOut := max(1, availableOut-64)
	if localAvailableOut > 0 {
		safeOut = max(1, min(availableOut, localAvailableOut)-64)
	}
	a.ephemeralMaxOutputTokens = safeOut
	a.bufferVprint(fmt.Sprintf("⚠️  Output cap too large for current prompt — retrying with max_tokens=%s (provider_available=%s, estimated_request_tokens=%s; context_length unchanged at %s)",
		groupThousands(safeOut), groupThousands(availableOut), groupThousands(requestInputEstimate), groupThousands(oldCtx)))
	if exhausted, err := r.countAttempt(false); err != nil || exhausted != nil {
		return derefVerdict(exhausted), err
	}
	deferred, err := r.compressForOutputCap(requestInputEstimate)
	if err != nil {
		return PhaseVerdict{}, err
	}
	if deferred != nil {
		return *deferred, nil
	}
	r.retry.RestartWithCompressedMessages = true
	return r.done("break", nil), nil
}

// Output cap compression that never turns the error fatal, a keyboard interrupt still ends the turn
func (r *overflowRecovery) compressForOutputCap(requestInputEstimate int) (deferred *PhaseVerdict, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if isUserInterruptPanic(rec) {
				panic(rec)
			}
			if e, ok := rec.(error); ok && tools.IsKeyboardInterrupt(r.ctx, e) {
				panic(rec)
			}
			deferred, err = nil, nil
			conversationLoopLog().Warn(fmt.Sprintf("%sOutput-cap compression hit an error; retrying on max_tokens only.", r.a.logPrefix))
		}
	}()
	v, _, _, cerr := r.compressScoredByTokens(requestInputEstimate, false)
	if cerr != nil {
		if tools.IsKeyboardInterrupt(r.ctx, cerr) {
			return nil, cerr
		}
		conversationLoopLog().Warn(fmt.Sprintf("%sOutput-cap compression hit an error; retrying on max_tokens only.", r.a.logPrefix))
		return nil, nil
	}
	return v, nil
}

// Adopts a context length the provider error reports, false when it reports none and the window stays
func adoptProviderContextLimit(ctx context.Context, a *AIAgent, errorMsg string, oldCtx int) (int, bool) {
	compressor := a.contextCompressor
	if newCtx, ok := provider.GetContextLengthFromProviderError(errorMsg, oldCtx); ok {
		a.bufferVprint(fmt.Sprintf("Context limit detected from API: %s tokens (was %s)", groupThousands(newCtx), groupThousands(oldCtx)))
		compressor.UpdateModel(a.model, newCtx, a.baseURL, a.apiKey, a.provider, a.apiMode)
		provider.SaveProviderContextLength(ctx, a.model, a.baseURL, newCtx, a.provider)
		if cc, ok := compressor.(*ContextCompressor); ok {
			cc.contextProbed, cc.contextProbePersistable = true, true
		}
		a.bufferVprint(fmt.Sprintf("⚠️  Context length exceeded — using provider limit: %s → %s tokens", groupThousands(oldCtx), groupThousands(newCtx)))
		return newCtx, true
	}
	providerLower := pystr.Lower(a.provider)
	baseLower := pystr.Lower(pystr.RstripChars(a.baseURL, "/"))
	isMinimax := providerLower == "minimax" || providerLower == "minimax-cn"
	for _, prefix := range minimaxAnthropicURLs {
		isMinimax = isMinimax || strings.HasPrefix(baseLower, prefix)
	}
	if isMinimax && strings.Contains(errorMsg, "context window exceeds limit (") {
		a.bufferVprint(fmt.Sprintf("Provider reported overflow amount only; keeping context_length at %s tokens.", groupThousands(oldCtx)))
	} else {
		a.bufferVprint(fmt.Sprintf("⚠️  Context length exceeded, but provider did not report a max context length; keeping context_length at %s tokens.", groupThousands(oldCtx)))
	}
	return 0, false
}

// Context length recovery, the output cap clamp for a stated budget else provider limit adoption and compression
func recoverContextLength(r *overflowRecovery, errorMsg string) (PhaseVerdict, error) {
	a, s := r.a, r.s
	oldCtx := a.contextCompressor.Base().ContextLength
	if availableOut, ok := provider.ParseAvailableOutputTokensFromError(errorMsg); ok {
		return clampOutputCap(r, availableOut, oldCtx)
	}
	if provider.IsOutputCapError(errorMsg) {
		return r.failTurn("The requested output length exceeds the provider's output cap for this model, and the error did not state the allowed limit.", overflowFailure{
			notices: []string{
				"❌ The provider rejected the request because the requested output length exceeds its output cap for this model, and the error did not state the allowed limit.",
				"   💡 Icebath has no user setting for the output cap — check the endpoint's default max output (completion) tokens for this model on the server or proxy. (This is an output-cap error, not a context overflow — compression cannot fix it.)",
			},
			log:      fmt.Sprintf("%sOutput-cap error not routed into compression (max_tokens over provider cap): %s", a.logPrefix, pystr.Slice(errorMsg, 0, 200)),
			softExit: true,
		})
	}
	newCtx, adopted := adoptProviderContextLimit(r.ctx, a, errorMsg, oldCtx)
	window := a.contextCompressor.Base().ContextLength
	requestTokens := r.requestTokens() + max(0, a.maxTokensOrZero())
	if provider.IsLocalEndpoint(a.baseURL) && !quotedTokenCountRe.MatchString(errorMsg) && window > 0 &&
		float64(requestTokens) < float64(window)*unexplainedRejectionFraction {
		return r.failWithCopy("server_context_rejection", map[string]any{"model": a.model, "tokens": requestTokens, "window": window}, overflowFailure{
			notices: []string{
				fmt.Sprintf("❌ The server rejected the request as too large, but it is only ~%s tokens against a %s-token window — not compressing.", groupThousands(requestTokens), groupThousands(window)),
				"   💡 Wait a moment and /retry; another request on the same server (e.g. a background review) may have been holding its context.",
			},
			log: fmt.Sprintf("%sProvider context rejection not explained by request size (~%s of %s tokens); skipping compression: %s",
				a.logPrefix, groupThousands(requestTokens), groupThousands(window), pystr.Slice(errorMsg, 0, 200)),
			softExit: true, reason: string(provider.ReasonServerError), retryable: true,
		})
	}
	if exhausted, err := r.countAttempt(false); err != nil || exhausted != nil {
		return derefVerdict(exhausted), err
	}
	a.BufferDiagnosticStatus(CompressionRetryTooLargeStatus(s.ApproxTokens, s.CompressionAttempts, s.MaxCompressionAttempts))
	deferred, shrank, newTokens, err := r.compressScoredByTokens(r.requestTokens(), true)
	if err != nil || deferred != nil {
		return derefVerdict(deferred), err
	}
	s.ApproxTokens = newTokens
	if shrank || (adopted && newCtx != 0 && newCtx < oldCtx) {
		recoverySleep(2 * time.Second)
		s.ProviderOverflowRecoveryPending = true
		r.retry.RestartWithCompressedMessages = true
		return r.done("break", nil), nil
	}
	return r.failWithCopy("context_overflow", map[string]any{"model": a.model}, overflowFailure{
		notices: []string{fmt.Sprintf("❌ The conversation is too long for the model (%s tokens) and cannot be shrunk further.", groupThousands(newTokens)), overflowRetryHint},
		log:     fmt.Sprintf("%sContext length exceeded: %s tokens. Cannot compress further.", a.logPrefix, groupThousands(newTokens)),
	})
}

// Every registered profile's rejection hint lines for this endpoint, whatever route reached it
func requestRejectionHint(ctx context.Context, statusCode *int, baseURL string) []string {
	status := 0
	if statusCode != nil {
		status = *statusCode
	}
	var out []string
	for _, profile := range provider.ListProviders(ctx) {
		out = append(out, profile.RequestRejectionHint(status, baseURL)...)
	}
	return out
}

// Payload too large and context length recovery, fallthrough when the error is neither or recovery declined
func recoverFromOverflow(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn) (PhaseVerdict, error) {
	r := &overflowRecovery{ctx: ctx, a: a, s: s, retry: s.Retry}
	reason := string(e.classified.Reason)
	r.overflowReason = &reason
	for _, line := range requestRejectionHint(ctx, e.statusCode, a.baseURL) {
		a.vprint(a.logPrefix+line, true, true)
	}
	e.isContextLengthError = false
	if e.classified.Reason == provider.ReasonPayloadTooLarge {
		return recoverPayloadTooLarge(r)
	}
	e.isContextLengthError = e.classified.Reason == provider.ReasonContextOverflow || e.wrappedOutputCapBudget != nil
	if e.isContextLengthError {
		return recoverContextLength(r, e.errorMsg)
	}
	return r.done("fallthrough", nil), nil
}

// Value of an optional verdict, the zero verdict when absent
func derefVerdict(v *PhaseVerdict) PhaseVerdict {
	if v == nil {
		return PhaseVerdict{}
	}
	return *v
}
