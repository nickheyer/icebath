package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Runs the pre API guard chain, Ollama context floor, overflow recheck arming, progress blocker, then preflight compression
func runPreflightGate(ctx context.Context, a *AIAgent, s *LoopState) (PhaseVerdict, error) {
	lastPressure := s.LastPreflightPressure
	s.LastPreflightPressure = nil
	if contextError := ollamaContextLimitError(a, s.RequestPressureTokens); contextError != "" {
		s.FinalResponse, s.Failed, s.TurnExitReason = contextError, true, "ollama_runtime_context_too_small"
		appendLiveMessage(&s.Messages, map[string]any{"role": "assistant", "content": contextError})
		a.emitDiagnosticStatus("❌ Ollama runtime context is too small for Icebath tool use")
		s.APICallCount--
		a.apiCallCount.Store(int64(s.APICallCount))
		if budget := a.iterationBudget.Load(); budget != nil {
			budget.Refund()
		}
		return PhaseVerdict{Action: "break"}, nil
	}
	compressor := a.contextCompressor
	threshold := 0
	if compressor != nil {
		threshold = compressor.Base().ThresholdTokens
	}
	overflowPreflight := s.ProviderOverflowRecoveryPending && (threshold <= 0 || s.RequestPressureTokens >= threshold)
	if s.ProviderOverflowRecoveryPending && !overflowPreflight {
		s.ProviderOverflowRecoveryPending = false
	}
	if lastPressure != nil && s.RequestPressureTokens >= threshold &&
		!compressionWarrantsAnotherPreflightPass(*lastPressure, s.RequestPressureTokens, threshold) {
		s.PreflightCompressionBlocked = true
		conversationLoopLog().Warn(fmt.Sprintf("Pre-API compression made insufficient progress: ~%s -> ~%s request tokens; skipping additional preflight passes",
			pystr.GroupThousands(int64(*lastPressure)), pystr.GroupThousands(int64(s.RequestPressureTokens))))
	}
	deferPreflight := func(int) bool { return false }
	if !a.requestPressureAnchored && compressor != nil {
		deferPreflight = compressor.ShouldDeferPreflightToRealUsage
	}
	return runPreflightCompression(ctx, a, s, compressor, s.RequestPressureTokens, overflowPreflight, deferPreflight)
}
