package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestSustainedOverloadEscalationOverloadBudgetSurvivesAFreshCompressorBoundToTheSameSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := caseSessionDB(t)
	sessionID := "OVERLOAD_FRESH_BIND"
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	msgs := msgRows(12)
	overloaded := func(t *testing.T) *ContextCompressor {
		c := abortCompressor(t, false)
		c.summaryModel = "test/auxiliary"
		return c
	}
	patchCallLLM(t, func(map[string]any) (any, error) {
		return nil, providerStatusError("Our servers are currently overloaded. Please try again later.", 503)
	})

	first := overloaded(t)
	first.bindSessionState(db, sessionID)
	for range 2 {
		if result, _ := first.Compress(msgs, iptr(999999), nil, true, ""); !sameRows(result, msgs) {
			t.Fatal("an overload below the escalation threshold must abort with messages unchanged")
		}
	}
	if streak, _ := db.CompressionOverloadStreak(sessionID); first.consecutiveOverloadAborts != 2 || streak != 2 {
		t.Fatalf("first aborts %d persisted %d", first.consecutiveOverloadAborts, streak)
	}

	second := overloaded(t)
	second.bindSessionState(db, sessionID)
	if second.consecutiveOverloadAborts != 2 {
		t.Fatalf("fresh compressor restarted the budget at %d", second.consecutiveOverloadAborts)
	}
	third, _ := second.Compress(msgs, iptr(999999), nil, true, "")
	if sameRows(third, msgs) || !second.LastSummaryFallbackUsed || !second.lastSummaryOverloadDegraded {
		t.Fatalf("third abort did not escalate fallback=%v degraded=%v", second.LastSummaryFallbackUsed, second.lastSummaryOverloadDegraded)
	}
	if fc, _ := second.Last.Get("failure_class"); fc != "summary_overload_degraded" {
		t.Fatalf("failure class %v", fc)
	}
	second.RecordCompletedCompaction(true)
	if streak, err := db.CompressionOverloadStreak(sessionID); err != nil || streak != 0 {
		t.Fatalf("committed degraded fallback left streak %d %v", streak, err)
	}
}
