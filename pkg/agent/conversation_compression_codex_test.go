package agent

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

type fakeCodexCompactor struct {
	result *provider.CodexTurnResult
	calls  int
	closed bool
	before func()
}

func (f *fakeCodexCompactor) CompactThread(turnTimeout, notificationPollTimeout float64) (*provider.CodexTurnResult, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	return f.result, nil
}

func (f *fakeCodexCompactor) Close() { f.closed = true }

type recordingCooldownEngine struct {
	*stubEngine
	remaining float64
	recorded  [][2]any
}

func (e *recordingCooldownEngine) GetActiveCompressionFailureCooldown(refresh bool) map[string]any {
	if e.remaining <= 0 {
		return nil
	}
	return map[string]any{"remaining_seconds": e.remaining, "error": "prior failure"}
}

func (e *recordingCooldownEngine) recordCompressionFailureCooldown(seconds float64, errText *string) {
	e.recorded = append(e.recorded, [2]any{seconds, *errText})
	e.remaining = seconds
}

type touchRecorder struct {
	mu          sync.Mutex
	descs       []string
	provenances []state.ActivityProvenance
}

func (r *touchRecorder) seen(desc string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.descs, desc)
}

// Codex route agent with a fake session and every touch recorded through the kanban bridge
func newCodexCompactionAgent(t *testing.T, result *provider.CodexTurnResult, autoMode string) (*AIAgent, *fakeCodexCompactor, *touchRecorder) {
	t.Helper()
	a := &AIAgent{sessionID: "hermes-session-1", platform: "cli", codexAppServerAutoCompaction: autoMode}
	a.apiMode = "codex_app_server"
	a.cachedSystemPrompt = "cached prompt"
	a.compressionActivityHeartbeatInterval = 0.1
	a.contextCompressor = newStubEngine(200000, 0.5)
	session := &fakeCodexCompactor{result: result}
	prev := codexCompactionSession
	codexCompactionSession = func(*AIAgent) codexThreadCompactor { return session }
	rec := &touchRecorder{}
	prevHook := KanbanActivityHook
	KanbanActivityHook = func(agent *AIAgent) {
		rec.mu.Lock()
		rec.descs = append(rec.descs, agent.lastActivityDesc)
		rec.provenances = append(rec.provenances, agent.lastActivityProvenance)
		rec.mu.Unlock()
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "codex-compaction")
	t.Cleanup(func() { codexCompactionSession, KanbanActivityHook = prev, prevHook })
	return a, session, rec
}

func runCodexCompaction(t *testing.T, a *AIAgent, messages []map[string]any, force bool) ([]map[string]any, string) {
	t.Helper()
	system := "system"
	approx := 100000
	out, prompt, err := compressContextViaCodexAppServer(context.Background(), a, messages, &system, &approx, "test", force, time.Now(), nil)
	if err != nil {
		t.Fatalf("codex compaction: %v", err)
	}
	return out, prompt
}

func TestCodexAppServerNativeAutoModeLeavesThreadCompactionToCodex(t *testing.T) {
	a, session, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1"}, "native")
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	out, prompt := runCodexCompaction(t, a, messages, false)
	if !sameMessageList(out, messages) || prompt != "cached prompt" || session.calls != 0 || a.contextCompressor.Base().CompressionCount != 0 {
		t.Fatalf("native mode compacted: calls=%d prompt=%q", session.calls, prompt)
	}
}

func TestCodexAppServerCompactionHeartbeatRefreshesActivityWhileWaiting(t *testing.T) {
	a, session, rec := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1"}, "native")
	session.before = func() {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && !rec.seen("context compression in progress") {
			time.Sleep(10 * time.Millisecond)
		}
		if !rec.seen("context compression in progress") {
			t.Errorf("no in progress heartbeat while the compaction waited")
		}
	}
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	out, prompt := runCodexCompaction(t, a, messages, true)
	if !sameMessageList(out, messages) || prompt != "cached prompt" || session.calls != 1 {
		t.Fatalf("forced compaction: calls=%d prompt=%q", session.calls, prompt)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Contains(rec.descs, "context compression started") || rec.descs[len(rec.descs)-1] != "context compression completed" {
		t.Fatalf("touches = %v", rec.descs)
	}
	for _, p := range rec.provenances {
		if p != state.ActivityProvenanceAgentCompression {
			t.Fatalf("provenance %q in %v", p, rec.provenances)
		}
	}
}

func TestInterruptedCodexCompactionArmsTheFailureCooldown(t *testing.T) {
	a, _, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1", Interrupted: true, Error: "compact turn interrupted"}, "hermes")
	engine := &recordingCooldownEngine{stubEngine: newStubEngine(200000, 0.5)}
	a.contextCompressor = engine
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	out, prompt := runCodexCompaction(t, a, messages, false)
	if !sameMessageList(out, messages) || prompt != "cached prompt" {
		t.Fatalf("prompt %q", prompt)
	}
	if len(engine.recorded) != 1 || engine.recorded[0] != [2]any{float64(summaryFailureCooldownSeconds), "compact turn interrupted"} {
		t.Fatalf("recorded = %v", engine.recorded)
	}
}

func TestCodexCompactionErrorWithoutInterruptAlsoArmsCooldown(t *testing.T) {
	a, _, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1", Error: "boom"}, "hermes")
	engine := &recordingCooldownEngine{stubEngine: newStubEngine(200000, 0.5)}
	a.contextCompressor = engine
	runCodexCompaction(t, a, []map[string]any{{"role": "user", "content": "hi"}}, false)
	if len(engine.recorded) != 1 || engine.recorded[0][1] != "boom" {
		t.Fatalf("recorded = %v", engine.recorded)
	}
}

func TestActiveCooldownBlocksAutomaticCodexCompaction(t *testing.T) {
	a, session, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1"}, "hermes")
	a.contextCompressor = &recordingCooldownEngine{stubEngine: newStubEngine(200000, 0.5), remaining: 120}
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	out, prompt := runCodexCompaction(t, a, messages, false)
	if !sameMessageList(out, messages) || prompt != "cached prompt" || session.calls != 0 {
		t.Fatalf("compaction ran despite an active cooldown: calls=%d", session.calls)
	}
}

func TestForceBypassesTheCodexCompactionCooldown(t *testing.T) {
	a, session, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1"}, "native")
	a.contextCompressor = &recordingCooldownEngine{stubEngine: newStubEngine(200000, 0.5), remaining: 120}
	runCodexCompaction(t, a, []map[string]any{{"role": "user", "content": "hi"}}, true)
	if session.calls != 1 {
		t.Fatalf("calls = %d", session.calls)
	}
}

func TestSuccessfulCodexCompactionArmsNoCooldown(t *testing.T) {
	a, _, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", TurnID: "compact-turn-1"}, "hermes")
	engine := &recordingCooldownEngine{stubEngine: newStubEngine(200000, 0.5)}
	a.contextCompressor = engine
	runCodexCompaction(t, a, []map[string]any{{"role": "user", "content": "hi"}}, false)
	if len(engine.recorded) != 0 {
		t.Fatalf("recorded = %v", engine.recorded)
	}
}

func TestRetiringCodexCompactionClosesAndDropsTheSession(t *testing.T) {
	a, session, _ := newCodexCompactionAgent(t, &provider.CodexTurnResult{ThreadID: "thread-1", ShouldRetire: true}, "native")
	a.codexSession = &provider.CodexAppServerSession{}
	runCodexCompaction(t, a, []map[string]any{{"role": "user", "content": "hi"}}, true)
	if !session.closed || a.codexSession != nil {
		t.Fatalf("closed=%v session=%v", session.closed, a.codexSession)
	}
}

func TestResetReadDedupCachesForgetsTheScreenshotFrameOfTheSession(t *testing.T) {
	var got []string
	prev := ResetScreenshotDedup
	ResetScreenshotDedup = func(sessionID string) { got = append(got, sessionID) }
	t.Cleanup(func() { ResetScreenshotDedup = prev })
	resetReadDedupCaches("task", "")
	resetReadDedupCaches("task", "sess-1")
	if !slices.Equal(got, []string{"sess-1"}) {
		t.Fatalf("screenshot resets = %v", got)
	}
	ResetScreenshotDedup = nil
	resetReadDedupCaches("task", "sess-1")
}

func TestSummaryAndAuxFallbackWarningsAreLatchedPerDistinctFailure(t *testing.T) {
	c := NewContextCompressor(context.Background(), feasibilityCompressorOptions("test-model", 100000, 0.5, "lean"))
	a := &AIAgent{contextCompressor: c}
	warnSummaryOrAuxFallback(a)
	c.SetLastSummaryError("boom")
	seen := "boom"
	a.lastCompressionSummaryWarning = &seen
	warnSummaryOrAuxFallback(a)
	c.lastSummaryError = nil
	c.LastAuxModelFailureModel, c.LastAuxModelFailureError = "aux-model", "502"
	key := [2]string{"aux-model", "502"}
	a.lastAuxFallbackWarningKey = &key
	warnSummaryOrAuxFallback(a)
	if *a.lastCompressionSummaryWarning != "boom" || *a.lastAuxFallbackWarningKey != key {
		t.Fatalf("latches moved: %v %v", *a.lastCompressionSummaryWarning, *a.lastAuxFallbackWarningKey)
	}
}
