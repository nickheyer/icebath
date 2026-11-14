package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestLoggingSessionContextFollowsCompressionRotation(t *testing.T) {
	db := newRecoveryDB(t)
	parentSID := "PARENT_LOGCTX_SESSION"
	if err := db.CreateSession(state.NewSession{ID: parentSID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	prevSetup := agentSetupLogging
	agentSetupLogging = func(context.Context, string) error { return nil }
	t.Cleanup(func() { agentSetupLogging = prevSetup })
	a := newFixtureAgent(t, AIAgentOptions{BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", SessionDB: db, SessionID: parentSID})
	engine := &scriptedEngine{stubEngine: *newStubEngine(200000, 0.5), compress: func([]map[string]any) []map[string]any {
		return []map[string]any{{"role": "user", "content": "[CONTEXT COMPACTION] summary"}, {"role": "user", "content": "tail"}}
	}}
	engine.CompressionCount = 1
	a.contextCompressor = engine
	a.compressionInPlace = false
	h := &sessionTagHandler{}
	lg := logging.GetLogger("agent.test_compression_logging_session_context")
	lg.SetLevel(logging.LevelDebug)
	lg.SetPropagate(false)
	lg.AddHandler(h)
	t.Cleanup(func() { lg.RemoveHandler(h); lg.SetPropagate(true); logging.ClearSessionContext() })
	setLoggingSessionContext(parentSID)
	messages := make([]map[string]any, 20)
	for i := range messages {
		messages[i] = map[string]any{"role": "user", "content": fmt.Sprintf("m%d", i)}
	}
	approx := 120_000
	system := "sys"
	if _, _, err := a.compressContext(context.Background(), messages, &system, compressContextOptions{ApproxTokens: &approx}); err != nil {
		t.Fatal(err)
	}
	if a.sessionID == parentSID {
		t.Fatal("the session id did not rotate")
	}
	lg.Info("after rotation")
	h.mu.Lock()
	defer h.mu.Unlock()
	if want := " [" + a.sessionID + "]"; len(h.tags) != 1 || h.tags[0] != want {
		t.Fatalf("Logging session context did not follow the compaction rotation: log tag still %q, agent.session_id is %q (see #34089)", h.tags, a.sessionID)
	}
}
