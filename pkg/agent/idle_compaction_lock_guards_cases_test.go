package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Engine double of the concurrent fork fixture, counting compress calls and reporting a settable last pass size
type idleEngine struct {
	compactionEngine
	compressCalls int
	lastRough     int
}

func (e *idleEngine) ShouldCompress(*int) bool                 { return false }
func (e *idleEngine) ShouldDeferPreflightToRealUsage(int) bool { return false }
func (e *idleEngine) LastCompressionRoughTokens() int          { return e.lastRough }
func (e *idleEngine) Compress([]map[string]any, *int, *string, bool, string) ([]map[string]any, error) {
	e.compressCalls++
	return []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary"}, {"role": "user", "content": "tail"}}, nil
}

// Real compressor whose compress pass is counted instead of run
type countedCompressor struct {
	*ContextCompressor
	compressCalls int
}

func (c *countedCompressor) Compress([]map[string]any, *int, *string, bool, string) ([]map[string]any, error) {
	c.compressCalls++
	return nil, nil
}

// Real agent over db primed so the idle trigger is eligible after idleGap seconds of silence
func idleAgent(t *testing.T, db *state.SessionDB, sessionID string, idleAfter, idleGap float64) (*AIAgent, *idleEngine) {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", SessionDB: db, SessionID: sessionID})
	t.Cleanup(func() { a.Close() })
	engine := &idleEngine{compactionEngine: *newCompactionEngine(100_000, 0)}
	engine.ratio, engine.ProtectFirstN, engine.ProtectLastN, engine.CompressionCount = 0.20, 2, 2, 1
	a.contextCompressor, a.compressionFeasibilityChecked, a.compressionInPlace = engine, true, false
	a.compressionEnabled, a.compressionIdleCompactAfterSeconds = true, idleAfter
	a.lastActivityTS = loopNow() - idleGap
	a.cachedSystemPrompt = "SYSTEM"
	return a, engine
}

// Runs the turn prologue over history with the idle estimate pinned to roughTokens
func idlePrologue(t *testing.T, a *AIAgent, history []map[string]any, roughTokens int) *TurnContext {
	t.Helper()
	swapCompactionTokens(t, func(context.Context, *AIAgent, []*state.Message, string) int { return roughTokens })
	return prologueBuild(t, a, func(r *TurnContextRequest) { r.UserMessage, r.ConversationHistory = "hello again", history })
}

func idleHistory() []map[string]any {
	rows := make([]map[string]any, 20)
	for i := range rows {
		rows[i] = map[string]any{"role": "user", "content": fmt.Sprintf("m%d", i)}
	}
	return rows
}

// Fresh store with the session row created
func idleDB(t *testing.T, sessionID string) *state.SessionDB {
	t.Helper()
	db := blockDB(t, "state.db")
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestIdleCompactionLockAndGuards(t *testing.T) {
	caseHome(t)
	t.Run("test_idle_compaction_status_emitted_by_default", func(t *testing.T) {
		db := idleDB(t, "IDLE_LOUD")
		a, engine := idleAgent(t, db, "IDLE_LOUD", 60, 3600)
		engine.EmitAutomaticCompactionStatus = true
		var events [][2]string
		a.statusCallback = func(kind string, msg any) { events = append(events, [2]string{kind, statusText(msg)}) }
		idlePrologue(t, a, idleHistory(), 999_999)
		if engine.compressCalls != 1 {
			t.Fatalf("compress calls %d", engine.compressCalls)
		}
		found := false
		for _, ev := range events {
			found = found || ev[0] == "lifecycle" && strings.Contains(ev[1], "Resumed after")
		}
		if !found {
			t.Fatalf("expected idle status line, got: %q", events)
		}
	})
	t.Run("test_idle_compaction_defers_to_held_compression_lock", func(t *testing.T) {
		db := idleDB(t, "IDLE_LOCKED")
		if !db.TryAcquireCompressionLock("IDLE_LOCKED", "external_holder", 300) {
			t.Fatal("lock not acquired")
		}
		a, engine := idleAgent(t, db, "IDLE_LOCKED", 60, 3600)
		history := idleHistory()
		tc := idlePrologue(t, a, history, 999_999)
		holder, err := db.GetCompressionLockHolder("IDLE_LOCKED")
		if engine.compressCalls != 0 || a.sessionID != "IDLE_LOCKED" || err != nil || holder == nil || *holder != "external_holder" {
			t.Fatalf("compress %d session %q holder %v %v", engine.compressCalls, a.sessionID, holder, err)
		}
		if len(tc.Messages) != len(history)+1 || tc.CurrentTurnUserIdx != len(tc.Messages)-1 || tc.Messages[tc.CurrentTurnUserIdx].Content != "hello again" {
			t.Fatalf("messages %d idx %d", len(tc.Messages), tc.CurrentTurnUserIdx)
		}
	})
	t.Run("test_idle_compaction_skips_a_transcript_that_has_not_grown", func(t *testing.T) {
		db := idleDB(t, "IDLE_RECOMPACT")
		a, engine := idleAgent(t, db, "IDLE_RECOMPACT", 1, 747)
		engine.ThresholdTokens, engine.ratio, engine.EmitAutomaticCompactionStatus, engine.lastRough = 127_510, 0.20, true, 44_579
		seam := 0
		swapFacadeCompress(t, func(messages []map[string]any, _ *string, _ compressContextOptions) ([]map[string]any, string, error) {
			seam++
			return messages, "SYSTEM", nil
		})
		tc := idlePrologue(t, a, idleHistory(), 44_579)
		if seam != 0 || engine.compressCalls != 0 || a.sessionID != "IDLE_RECOMPACT" {
			t.Fatalf("seam %d compress %d session %q", seam, engine.compressCalls, a.sessionID)
		}
		if len(tc.Messages) != len(idleHistory())+1 || tc.CurrentTurnUserIdx != len(tc.Messages)-1 {
			t.Fatalf("messages %d idx %d", len(tc.Messages), tc.CurrentTurnUserIdx)
		}
	})
	t.Run("test_idle_compaction_respects_anti_thrash_breaker", func(t *testing.T) {
		db := idleDB(t, "IDLE_THRASH")
		a, _ := idleAgent(t, db, "IDLE_THRASH", 60, 3600)
		real := newCompressorWith(t, 100_000, func(o *ContextCompressorOptions) {
			o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = 0.85, 2, 2
		})
		if err := real.BindSessionState(db, "IDLE_THRASH"); err != nil {
			t.Fatal(err)
		}
		if err := db.SetCompressionIneffectiveCount("IDLE_THRASH", 2); err != nil {
			t.Fatal(err)
		}
		real.ineffectiveCompressionCount = 2
		compressor := &countedCompressor{ContextCompressor: real}
		a.contextCompressor = compressor
		tc := idlePrologue(t, a, idleHistory(), 999_999)
		if compressor.compressCalls != 0 || a.sessionID != "IDLE_THRASH" || len(tc.Messages) != len(idleHistory())+1 {
			t.Fatalf("compress %d session %q messages %d", compressor.compressCalls, a.sessionID, len(tc.Messages))
		}
	})
}
