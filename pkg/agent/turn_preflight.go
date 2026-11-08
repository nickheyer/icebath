package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Pre API compression gate rebinding the loop state, refunding the call on every pass that never reached the provider
func runPreflightCompression(ctx context.Context, a *AIAgent, s *LoopState, compressor ContextEngine, requestPressureTokens int, providerOverflowPreflight bool, deferPreflight func(int) bool) (PhaseVerdict, error) {
	tokens := requestPressureTokens
	exhausted := func() (PhaseVerdict, error) {
		result, err := providerOverflowExhaustedResult(ctx, a, s.Messages, s.ConversationHistory, s.APICallCount, tokens, s.MaxCompressionAttempts)
		if err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "return", Result: result}, nil
	}
	threshold, contextLength := 0, 0
	if compressor != nil {
		threshold, contextLength = compressor.Base().ThresholdTokens, compressor.Base().ContextLength
	}
	cooldown := activeCompressionFailureCooldown(compressor)
	eligible := a.compressionEnabled && len(s.Messages) > 1 && s.CompressionAttempts < s.MaxCompressionAttempts
	if eligible {
		ensureCompressionFeasibilityChecked(ctx, a, tokens)
	}
	switch {
	case eligible && !reviewForkFirstRequestPending(a) && (!s.PreflightCompressionBlocked || providerOverflowPreflight) &&
		(!deferPreflight(tokens) || providerOverflowPreflight) && len(cooldown) == 0 && compressor.ShouldCompress(&tokens):
		if grown, ok := maybeGrowLocalWindow(a, compressor, tokens); ok && grown != 0 {
			applyGrownWindow(a, compressor, grown)
			s.APICallCount = refundAPICall(a, s.APICallCount)
			return PhaseVerdict{Action: "continue"}, nil
		}
		if s.MoaPreparedRequest != nil {
			s.PendingMoaPreparedRequest = s.MoaPreparedRequest
		}
		s.CompressionAttempts++
		clearOverflowWarn(a)
		ctxText := "unknown"
		if contextLength != 0 {
			ctxText = groupThousands(contextLength)
		}
		conversationLoopLog().Info(fmt.Sprintf("Pre-API compression: ~%s request tokens >= %s threshold (context=%s, attempt=%d/%d)",
			groupThousands(tokens), groupThousands(threshold), ctxText, s.CompressionAttempts, s.MaxCompressionAttempts))
		status := AutomaticCompactionStatusMessage(compressor, "pre_api", preAPICompressionStatus(tokens), map[string]any{
			"approx_tokens": tokens, "threshold_tokens": threshold, "context_length": contextLength, "model": a.model,
			"attempt": s.CompressionAttempts, "max_attempts": s.MaxCompressionAttempts,
		})
		if status != nil {
			a.emitStatus(*status)
		}
		pressure := tokens
		s.LastPreflightPressure = &pressure
		lc, err := compactLive(ctx, a, s.Messages, s.SystemMessage, tokens, s.EffectiveTaskID, "pre_api")
		if err != nil {
			return PhaseVerdict{}, err
		}
		s.Messages, s.ActiveSystemPrompt = lc.messages, lc.system
		if contextCompressionTimedOut(a) {
			s.APICallCount = refundAPICall(a, s.APICallCount)
			s.FinalResponse, s.Failed = compressionTimeoutFinalResponse, true
			s.CompressionTimeoutExhausted, s.TurnExitReason = true, "context_compression_timeout"
			return PhaseVerdict{Action: "break"}, nil
		}
		if lc.skipped && (compressionSkippedDueToLock(a) || compressionBlockedTransiently(a)) {
			s.CompressionAttempts--
			s.LastPreflightPressure = nil
			if sameMapObject(s.PendingMoaPreparedRequest, s.MoaPreparedRequest) {
				s.PendingMoaPreparedRequest = nil
			}
			break
		}
		resetRetryStateAfterCompaction(a)
		s.ConversationHistory = historyAfterCompaction(a, lc, s.ConversationHistory)
		s.APICallCount = refundAPICall(a, s.APICallCount)
		if shouldSkipModelCallForReferenceHandoff(&s.Messages, s.UserMessage) {
			conversationLoopLog().Info("Skipping post-compaction model call: reference-only handoff would be the sole active user turn (#80622)")
			if !pyTruthy(s.FinalResponse) {
				s.FinalResponse = handoffSkipFinalResponse
			}
			s.TurnExitReason = "compaction_handoff_not_actionable"
			return PhaseVerdict{Action: "break"}, nil
		}
		return PhaseVerdict{Action: "continue"}, nil
	case providerOverflowPreflight && len(cooldown) > 0:
		if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
			return PhaseVerdict{}, err
		}
		return PhaseVerdict{Action: "return", Result: compressionDeferredResult(a, s.Messages, s.APICallCount, "transient_block")}, nil
	case providerOverflowPreflight && s.CompressionAttempts >= s.MaxCompressionAttempts:
		return exhausted()
	case eligible && !deferPreflight(tokens) && len(cooldown) > 0:
		if reason := blockedCompressReason(compressor, tokens, nil); reason != nil && *reason != "" {
			a.warnContextOverflowBlocked(*reason, tokens, threshold)
		}
	case !a.compressionEnabled && len(s.Messages) > 1:
		if a.contextCompressor != nil {
			if ctxLen := a.contextCompressor.Base().ContextLength; ctxLen > 0 && tokens > ctxLen {
				a.warnUncompressedContextOverflow(tokens, ctxLen)
			}
		}
	}
	if providerOverflowPreflight {
		return exhausted()
	}
	return PhaseVerdict{}, nil
}

// Post tool compression on real prompt pressure, else the blocked warning and tool result prune, true ends the turn
func compressAfterToolResults(ctx context.Context, a *AIAgent, s *LoopState) (bool, error) {
	compressor := a.contextCompressor
	if awaitingRealUsageAfterCompression(compressor) {
		return false, nil
	}
	base := compressor.Base()
	var realTokens int
	if anchored, ok := provider.AnchoredContextTokens(ctx, s.Messages, a.usageAnchor, true); ok {
		realTokens = anchored
	} else if base.LastPromptTokens > 0 {
		realTokens = base.LastPromptTokens
	} else if base.LastPromptTokens == -1 {
		realTokens = 0
	} else {
		var toolDefs []map[string]any
		if len(a.tools) > 0 {
			toolDefs = a.ToolDefs()
		}
		rough := provider.EstimateRequestTokens(ctx, s.Messages, "", toolDefs, true)
		realTokens = midturnRequestPressureTokens(ctx, a, liveMessagesAsAPIRows(s.Messages), s.ActiveSystemPrompt, rough)
	}
	if a.compressionEnabled && s.CompressionAttempts < s.MaxCompressionAttempts {
		ensureCompressionFeasibilityChecked(ctx, a, realTokens)
	}
	if a.compressionEnabled && s.CompressionAttempts < s.MaxCompressionAttempts && !awaitingRealUsageAfterCompression(compressor) && compressor.ShouldCompress(&realTokens) {
		s.CompressionAttempts++
		clearOverflowWarn(a)
		a.safePrint("  ⟳ compacting context…", false)
		lc, err := compactLive(ctx, a, s.Messages, s.SystemMessage, realTokens, s.EffectiveTaskID, "post_tool")
		if err != nil {
			return false, err
		}
		s.Messages, s.ActiveSystemPrompt = lc.messages, lc.system
		if lc.skipped && compressionSkippedDueToLock(a) {
			s.CompressionAttempts--
			return false, nil
		}
		s.ConversationHistory = historyAfterCompaction(a, lc, s.ConversationHistory)
		if shouldSkipModelCallForReferenceHandoff(&s.Messages, s.UserMessage) {
			conversationLoopLog().Info("Skipping post-tool compaction model call: reference-only handoff would be the sole active user turn (#80622)")
			if !pyTruthy(s.FinalResponse) {
				s.FinalResponse = handoffSkipFinalResponse
			}
			s.TurnExitReason = "compaction_handoff_not_actionable"
			return true, nil
		}
		s.CurrentTurnUserIdx = reanchor(a, s.Messages, s.UserMessage)
		return false, nil
	}
	if !a.compressionEnabled {
		return false, nil
	}
	attempts := s.CompressionAttempts
	if reason := blockedCompressReason(compressor, realTokens, &attempts); reason != nil && *reason != "" {
		a.warnContextOverflowBlocked(*reason, realTokens, base.ThresholdTokens)
	}
	rows := messagesToRows(s.Messages)
	pruned, n := func() (pruned []map[string]any, n int) {
		defer func() {
			if r := recover(); r != nil {
				if isUserInterruptPanic(r) {
					panic(r)
				}
				conversationLoopLog().Debug(fmt.Sprintf("proactive tool-result prune failed; skipping: %v", r))
				pruned, n = rows, 0
			}
		}()
		return compressor.PruneToolResultsOnly(rows, &realTokens)
	}()
	if n != 0 && !sameRowSlice(pruned, rows) {
		prunedMessages := rowsToMessages(pruned)
		PublishCompactionPrune(ctx, a, s.Messages, prunedMessages, n)
		s.Messages = prunedMessages
		resetReadDedupCaches(ctx, s.EffectiveTaskID, a.sessionID)
	}
	return false, nil
}

// Live messages as the API row list the mid turn estimator reads
func liveMessagesAsAPIRows(messages []*state.Message) []any {
	out := make([]any, len(messages))
	for i, m := range messages {
		out[i] = m
	}
	return out
}
