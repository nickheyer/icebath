package gatewayrun

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// Real agent left with the previous turn's compression activity and ten API calls
func staleTurnAgent(t *testing.T) *agent.AIAgent {
	t.Helper()
	a := quietAgent(t)
	a.TouchActivityAs("previous turn activity", string(state.ActivityProvenanceAgentCompression))
	a.SetAPICallCount(10)
	return a
}

// Activity time, description and provenance of the agent
func activityTriple(a *agent.AIAgent) (any, any, any) {
	s := a.GetActivitySummary()
	return s["last_activity_ts"], s["last_activity_desc"], s["last_activity_provenance"]
}

func TestFreshTurnResetsIdleClock(t *testing.T) {
	a := staleTurnAgent(t)
	oldTS := a.LastActivityTS()
	initCachedAgentForTurn(a, 0)
	if a.LastActivityTS() <= oldTS {
		t.Fatal("_last_activity_ts was not reset on a fresh turn (interrupt_depth=0)")
	}
	if n := a.GetActivitySummary()["api_call_count"]; n != 0 {
		t.Fatalf("api_call_count %v", n)
	}
}

func TestFreshTurnResetsProvenance(t *testing.T) {
	a := staleTurnAgent(t)
	if _, _, p := activityTriple(a); p != string(state.ActivityProvenanceAgentCompression) {
		t.Fatalf("fixture provenance %v", p)
	}
	initCachedAgentForTurn(a, 0)
	if _, _, p := activityTriple(a); p != string(state.ActivityProvenanceUnknown) {
		t.Fatalf("provenance %v", p)
	}
}

func TestInterruptTurnPreservesIdleClock(t *testing.T) {
	a := staleTurnAgent(t)
	oldTS := a.LastActivityTS()
	initCachedAgentForTurn(a, 1)
	if a.LastActivityTS() != oldTS {
		t.Fatal("_last_activity_ts must not be reset on interrupt-recursive turns")
	}
}

func TestInterruptTurnPreservesDesc(t *testing.T) {
	a := staleTurnAgent(t)
	initCachedAgentForTurn(a, 1)
	if _, d, _ := activityTriple(a); d != "previous turn activity" {
		t.Fatalf("desc %v", d)
	}
}

func TestInterruptTurnPreservesProvenance(t *testing.T) {
	a := staleTurnAgent(t)
	initCachedAgentForTurn(a, 1)
	if _, _, p := activityTriple(a); p != string(state.ActivityProvenanceAgentCompression) {
		t.Fatalf("provenance %v", p)
	}
}

func TestFreshTurnResetsFlushCursor(t *testing.T) {
	a := staleTurnAgent(t)
	a.SetLastFlushedDBIdx(42)
	initCachedAgentForTurn(a, 0)
	if a.LastFlushedDBIdx() != 0 {
		t.Fatal("_last_flushed_db_idx must be reset on a fresh turn")
	}
}
