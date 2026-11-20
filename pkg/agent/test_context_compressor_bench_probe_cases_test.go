package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestBenchProbeDeadlineSurvivesACompressorRebuild(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := caseSessionDB(t, "s1")
	if err := db.SetCompressionFallbackStreak("s1", 2); err != nil {
		t.Fatal(err)
	}
	build := func() *ContextCompressor {
		return newCompressorWith(t, 100000, func(o *ContextCompressorOptions) {
			o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.85, 2, 2
		})
	}
	messages := func() []map[string]any {
		msgs := []map[string]any{{"role": "system", "content": "system prompt"}}
		for i := range 10 {
			msgs = append(msgs, map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)}, map[string]any{"role": "assistant", "content": "short reply"})
		}
		return msgs
	}
	now := 0.0
	prevNow, prevGen := compressorWallNow, compressorGenerateSummary
	t.Cleanup(func() { compressorWallNow, compressorGenerateSummary = prevNow, prevGen })
	compressorWallNow = func() float64 { return now }
	calls := 0
	compressorGenerateSummary = func(*ContextCompressor, context.Context, []map[string]any, *string, string, bool) (*string, error) {
		calls++
		s := "LLM summary"
		return &s, nil
	}
	compressor := build()
	compressor.bindSessionState(db, "s1")
	window := antiThrashRecoverySeconds

	now = 1000.0
	compressor.Compress(messages(), nil, nil, false, "")
	if calls != 0 {
		t.Fatalf("benched compress called the summary model %d times", calls)
	}

	rebuilt := build()
	rebuilt.bindSessionState(db, "s1")
	now = 1000.0 + window + 1
	rebuilt.Compress(messages(), nil, nil, false, "")
	if calls != 1 {
		t.Fatalf("rebuilt compressor probed %d times", calls)
	}

	now = 2000.0
	rebuilt.Compress(messages(), nil, nil, false, "")
	if err := db.CreateSession(state.NewSession{ID: "s2", Source: "cli", ParentSessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	rebuilt.OnSessionStart("s2", map[string]any{"boundary_reason": "compression", "old_session_id": "s1", "session_db": db})
	calls = 0
	now = 2000.0 + window + 1
	rebuilt.Compress(messages(), nil, nil, false, "")
	if calls != 1 {
		t.Fatalf("rotated child probed %d times", calls)
	}
}
