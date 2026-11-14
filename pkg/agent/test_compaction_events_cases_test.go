package agent

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestPruneWithoutRelayTargetSkipsTranscriptEstimates(t *testing.T) {
	caseHome(t)
	a := spAgent(t)
	a.contextCompressor = compactionCaseCompressor(t, nil)
	a.sessionID, a.platform, a.model, a.provider = "session-events-test", "cli", "test/main-model", "test-provider"
	a.compressionFeasibilityChecked = true
	messages := rowsToMessages(compactionCaseMessages())
	prevTarget, prevEstimate := compactionTargetAvailable, compactionEstimateRough
	t.Cleanup(func() { compactionTargetAvailable, compactionEstimateRough = prevTarget, prevEstimate })
	compactionTargetAvailable = func(context.Context, string, string) (bool, error) { return false, nil }
	estimates := 0
	compactionEstimateRough = func(context.Context, []*state.Message) int {
		estimates++
		return 0
	}
	PublishCompactionPrune(context.Background(), a, messages, messages[:len(messages)-1], 1)
	if estimates != 0 {
		t.Fatalf("estimate called %d times", estimates)
	}
}
