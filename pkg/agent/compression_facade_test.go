package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func newFenceRaceAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_SESSION_ID", "")
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: "S", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionDB: db, sessionID: "S", platform: "telegram", model: "test/model", provider: "openrouter", baseURL: "https://openrouter.ai/api/v1"}
	a.contextCompressor = newStubEngine(200000, 0.5)
	return a
}

type overlappingAttempts struct {
	fenceA, fenceB     *CompressionCommitFence
	enteredA, enteredB chan struct{}
	releaseA, releaseB chan struct{}
	mu                 sync.Mutex
	errs               []error
}

func newOverlappingAttempts(t *testing.T) *overlappingAttempts {
	o := &overlappingAttempts{
		fenceA: NewCompressionCommitFence(), fenceB: NewCompressionCommitFence(),
		enteredA: make(chan struct{}), enteredB: make(chan struct{}),
		releaseA: make(chan struct{}), releaseB: make(chan struct{}),
	}
	orig := facadeCompressContext
	facadeCompressContext = func(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, opts compressContextOptions) ([]map[string]any, string, error) {
		wait := func(entered, release chan struct{}, name string) {
			close(entered)
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				o.mu.Lock()
				o.errs = append(o.errs, fmt.Errorf("attempt %s never released", name))
				o.mu.Unlock()
			}
		}
		switch opts.CommitFence {
		case o.fenceA:
			wait(o.enteredA, o.releaseA, "A")
		case o.fenceB:
			wait(o.enteredB, o.releaseB, "B")
		}
		return []map[string]any{{"role": "user", "content": "x"}}, "prompt", nil
	}
	t.Cleanup(func() { facadeCompressContext = orig })
	return o
}

func (o *overlappingAttempts) attempt(a *AIAgent, fence *CompressionCommitFence, messages []map[string]any, done *sync.WaitGroup) {
	done.Add(1)
	go func() {
		defer done.Done()
		system := "sys"
		if _, _, err := a.compressContext(context.Background(), messages, &system, compressContextOptions{TaskID: "default", CommitFence: fence}); err != nil {
			o.mu.Lock()
			o.errs = append(o.errs, err)
			o.mu.Unlock()
		}
	}()
}

func waitEntered(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("attempt never entered compress_context")
	}
}

func joinWithin(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attempt never finished")
	}
}

func TestEarlierAttemptFinishingFirstKeepsNewerLiveFence(t *testing.T) {
	a := newFenceRaceAgent(t)
	messages := []map[string]any{{"role": "user", "content": "live question"}}
	o := newOverlappingAttempts(t)
	var wgA, wgB sync.WaitGroup
	o.attempt(a, o.fenceA, messages, &wgA)
	waitEntered(t, o.enteredA)
	o.attempt(a, o.fenceB, messages, &wgB)
	waitEntered(t, o.enteredB)
	if a.publishedCompressionCommitFence() != o.fenceB {
		t.Fatal("newest attempt's fence not published")
	}
	close(o.releaseA)
	joinWithin(t, &wgA)
	if a.publishedCompressionCommitFence() != o.fenceB {
		t.Fatal("earlier completion deleted the live newer fence")
	}
	close(o.releaseB)
	joinWithin(t, &wgB)
	if a.publishedCompressionCommitFence() != nil {
		t.Fatal("fence slot not cleared after the last attempt")
	}
	if len(o.errs) != 0 {
		t.Fatal(o.errs)
	}
}

func TestLifoCompletionRestoresOuterFenceWhileItRuns(t *testing.T) {
	a := newFenceRaceAgent(t)
	messages := []map[string]any{{"role": "user", "content": "live question"}}
	o := newOverlappingAttempts(t)
	var wgA, wgB sync.WaitGroup
	o.attempt(a, o.fenceA, messages, &wgA)
	waitEntered(t, o.enteredA)
	o.attempt(a, o.fenceB, messages, &wgB)
	waitEntered(t, o.enteredB)
	close(o.releaseB)
	joinWithin(t, &wgB)
	if a.publishedCompressionCommitFence() != o.fenceA {
		t.Fatal("outer attempt's fence not restored")
	}
	close(o.releaseA)
	joinWithin(t, &wgA)
	if a.publishedCompressionCommitFence() != nil {
		t.Fatal("fence slot not cleared after the last attempt")
	}
	if len(o.errs) != 0 {
		t.Fatal(o.errs)
	}
}

func captureNewFence(t *testing.T) *compressionTimeoutRun {
	captured := &compressionTimeoutRun{}
	orig := facadeRunWithProgressTimeout
	facadeRunWithProgressTimeout = func(ctx context.Context, o compressionTimeoutRun) ([]map[string]any, string, error) {
		*captured = o
		return []map[string]any{{"role": "user", "content": "m"}}, "p", nil
	}
	t.Cleanup(func() { facadeRunWithProgressTimeout = orig })
	return captured
}

func driveStallFallback(t *testing.T, a *AIAgent, registration *commitFenceRegistration, fence *CompressionCommitFence) func() (*CompressionCommitFence, error) {
	captured := captureNewFence(t)
	run := func(context.Context, *CompressionCommitFence, []map[string]any, bool) ([]map[string]any, string, error) {
		return []map[string]any{{"role": "user", "content": "m"}}, "p", nil
	}
	system := "sys"
	if _, _, err := runUnderProgressTimeout(context.Background(), a, run, []map[string]any{{"role": "user", "content": "x"}}, &system, fence, registration, 1, 2, nil); err != nil {
		t.Fatal(err)
	}
	if captured.NewFence == nil {
		t.Fatal("runner received no fence factory")
	}
	return captured.NewFence
}

func TestRetryFencePublishesWhileAttemptOwnsSlot(t *testing.T) {
	a := &AIAgent{}
	fence := NewCompressionCommitFence()
	registration := &commitFenceRegistration{fence: fence}
	a.compressionCommitFenceLock.Lock()
	a.compressionCommitFenceStack = []*commitFenceRegistration{registration}
	a.activeCompressionCommitFence = fence
	a.compressionCommitFenceLock.Unlock()

	retry, err := driveStallFallback(t, a, registration, fence)()
	if err != nil || retry == nil {
		t.Fatalf("retry fence %v %v", retry, err)
	}
	if registration.fence != retry || a.publishedCompressionCommitFence() != retry {
		t.Fatal("retry fence not swapped into the owning registration and the slot")
	}
}

func TestRetryFenceDoesNotClobberNewerAttempt(t *testing.T) {
	a := &AIAgent{}
	fence := NewCompressionCommitFence()
	registration := &commitFenceRegistration{fence: fence}
	otherFence := NewCompressionCommitFence()
	other := &commitFenceRegistration{fence: otherFence}
	a.compressionCommitFenceLock.Lock()
	a.compressionCommitFenceStack = []*commitFenceRegistration{registration, other}
	a.activeCompressionCommitFence = otherFence
	a.compressionCommitFenceLock.Unlock()

	retry, err := driveStallFallback(t, a, registration, fence)()
	if err != nil || retry == nil {
		t.Fatalf("retry fence %v %v", retry, err)
	}
	if registration.fence != retry {
		t.Fatal("retry fence not swapped into the owning registration")
	}
	if a.publishedCompressionCommitFence() != otherFence {
		t.Fatal("retry fence clobbered the newer live attempt")
	}
}

type terminalActivitySession struct {
	db        *state.SessionDB
	path      string
	sessionID string
	agent     *AIAgent
	warnings  []string
}

func newTerminalActivitySession(t *testing.T) *terminalActivitySession {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := state.NewSessionDB(t.Context(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &terminalActivitySession{db: db, path: path, sessionID: "COMPRESSION_TERMINAL_ACTIVITY"}
	if err := db.CreateSession(state.NewSession{ID: s.sessionID, Source: "test"}); err != nil {
		t.Fatal(err)
	}
	s.agent = &AIAgent{sessionDB: db, sessionID: s.sessionID, platform: "cli", model: "test/model"}
	s.agent.contextCompressor = newStubEngine(200000, 0.5)
	s.agent.statusCallback = func(kind, text string) {
		if kind == "warn" {
			s.warnings = append(s.warnings, text)
		}
	}
	return s
}

func activityRow(t *testing.T, db *state.SessionDB, sessionID string) (desc, provenance, endedAt any) {
	t.Helper()
	row, err := db.GetSession(context.Background(), sessionID)
	if err != nil || row == nil {
		t.Fatalf("row %v err=%v", row, err)
	}
	desc, _ = row.Get("last_activity_description")
	provenance, _ = row.Get("last_activity_provenance")
	endedAt, _ = row.Get("ended_at")
	return desc, provenance, endedAt
}

func midCompressionDurableStamp(t *testing.T, s *terminalActivitySession) {
	t.Helper()
	heartbeat := newCompressionActivityHeartbeat(s.agent, 3600.0, false, nil)
	s.agent.sessionActivityLastPersistMono = 0
	heartbeat.touch("context compression in progress", false, false)
	desc, provenance, _ := activityRow(t, s.db, s.sessionID)
	if desc != "context compression in progress" || provenance != string(state.ActivityProvenanceAgentCompression) {
		t.Fatalf("heartbeat stamp %v %v", desc, provenance)
	}
	s.agent.sessionActivityLastPersistMono = activityMonoNow()
}

func TestHostTimeoutStampIsDurableInsidePersistWindow(t *testing.T) {
	s := newTerminalActivitySession(t)
	midCompressionDurableStamp(t, s)
	reportCompressionTimeout(s.agent, 30, 30, 30, 60, false, false)
	desc, provenance, _ := activityRow(t, s.db, s.sessionID)
	if desc == "context compression in progress" || provenance != string(state.ActivityProvenanceAgentCompressionTimeout) {
		t.Fatalf("durable row %v %v", desc, provenance)
	}
	want := "⚠ Context compression timed out after 30.0s with no output from the summary model. No messages were dropped — continuing without compression. Run /compress to retry, /new for a clean session, or check auxiliary.compression."
	if len(s.warnings) != 1 || s.warnings[0] != want || !contextCompressionTimedOut(s.agent) {
		t.Fatalf("warnings %q timed out %v", s.warnings, contextCompressionTimedOut(s.agent))
	}
}

func TestStallInterruptedCooldownStampIsDurableInsidePersistWindow(t *testing.T) {
	s := newTerminalActivitySession(t)
	midCompressionDurableStamp(t, s)
	s.agent.warnContextOverflowBlocked("cooldown:42s", 144_323, 128_000)
	desc, provenance, _ := activityRow(t, s.db, s.sessionID)
	if desc == "context compression in progress" || provenance != string(state.ActivityProvenanceAgentCompressionCooldown) {
		t.Fatalf("durable row %v %v", desc, provenance)
	}
}

func TestReconnectReaderSeesTerminalStateNotInProgress(t *testing.T) {
	s := newTerminalActivitySession(t)
	midCompressionDurableStamp(t, s)
	reportCompressionTimeout(s.agent, 30, 90, 30, 90, true, false)
	reconnected, err := state.NewSessionDB(t.Context(), s.path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	desc, provenance, _ := activityRow(t, reconnected, s.sessionID)
	if desc == "context compression in progress" || provenance != string(state.ActivityProvenanceAgentCompressionTimeout) {
		t.Fatalf("reconnected row %v %v", desc, provenance)
	}
	want := "⚠ Context compression reached its total ceiling after 90.0s. No messages were dropped — continuing without compression. Run /compress to retry or /new for a clean session."
	if len(s.warnings) != 1 || s.warnings[0] != want {
		t.Fatalf("warnings %q", s.warnings)
	}
}

func TestOrphanReapConvergesTerminatedCompressionRow(t *testing.T) {
	s := newTerminalActivitySession(t)
	midCompressionDurableStamp(t, s)
	reportCompressionTimeout(s.agent, 30, 30, 30, 60, false, false)
	raw, err := sql.Open("sqlite", s.path)
	if err != nil {
		t.Fatal(err)
	}
	old := float64(time.Now().Unix() - 86_400)
	if _, err := raw.Exec("UPDATE sessions SET source = 'tui', started_at = ?, last_activity_at = ? WHERE id = ?", old, old, s.sessionID); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	reaped, err := s.db.SweepOrphanedSessions(t.Context(), state.SweepOrphanedOptions{MaxIdleSeconds: 3600, Sources: []string{"tui"}, RespectGatewayHeartbeats: false})
	if err != nil || !slices.Contains(reaped, s.sessionID) {
		t.Fatalf("reaped %v err=%v", reaped, err)
	}
	desc, _, endedAt := activityRow(t, s.db, s.sessionID)
	if endedAt == nil || desc == "context compression in progress" {
		t.Fatalf("reaped row ended=%v desc=%v", endedAt, desc)
	}
}

func TestCompressContextPreservesAmbientContext(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{})
	t.Setenv("ICEBATH_SESSION_ID", "")
	var seen []string
	orig := facadeCompressContext
	facadeCompressContext = func(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error) {
		seen = append(seen, provider.PortalConversationContext(ctx))
		return []map[string]any{}, "", nil
	}
	t.Cleanup(func() { facadeCompressContext = orig })
	a := &AIAgent{cachedConversationRoot: "segment-after-compaction"}
	system := "sys"
	ctx := provider.WithPortalConversationContext(context.Background(), "outer-root")
	if _, _, err := a.compressContext(ctx, []map[string]any{}, &system, compressContextOptions{TaskID: "default"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "outer-root" || provider.PortalConversationContext(ctx) != "outer-root" {
		t.Fatalf("ambient root %q after %q", seen, provider.PortalConversationContext(ctx))
	}
	if _, _, err := a.compressContext(context.Background(), []map[string]any{}, &system, compressContextOptions{TaskID: "default"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[1] != "segment-after-compaction" {
		t.Fatalf("out of turn compaction did not publish the root: %q", seen)
	}
}

func TestCompressContextWorkerNeverTouchesTheLiveTranscript(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{})
	t.Setenv("ICEBATH_SESSION_ID", "")
	orig := facadeCompressContext
	facadeCompressContext = func(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error) {
		messages[0]["content"] = "rewritten by a late engine"
		messages[0]["role"] = "assistant"
		return messages, "prompt", nil
	}
	t.Cleanup(func() { facadeCompressContext = orig })
	a := &AIAgent{sessionID: "S"}
	live := []map[string]any{{"role": "user", "content": "live question"}}
	system := "sys"
	out, prompt, err := a.compressContext(context.Background(), live, &system, compressContextOptions{TaskID: "default"})
	if err != nil || prompt != "prompt" || !sameMessageList(out, live) {
		t.Fatalf("out=%v prompt=%q err=%v", out, prompt, err)
	}
	if live[0]["content"] != "live question" || live[0]["role"] != "user" {
		t.Fatalf("pooled worker mutated the live transcript: %v", live)
	}
}

func TestCompressContextMirrorsPersistedStampsAndRebindsTheCaller(t *testing.T) {
	t.Setenv("ICEBATH_SESSION_ID", "")
	orig := facadeCompressContext
	facadeCompressContext = func(ctx context.Context, a *AIAgent, messages []map[string]any, systemMessage *string, o compressContextOptions) ([]map[string]any, string, error) {
		a.sessionID = "child"
		return []map[string]any{{"role": "user", "content": "kept turn", dbPersistedMarker: true}, {"role": "assistant", "content": "summary"}}, "prompt", nil
	}
	t.Cleanup(func() { facadeCompressContext = orig })
	h := &sessionTagHandler{}
	lg := logging.GetLogger("agent.test_facade_rebind")
	lg.SetLevel(logging.LevelDebug)
	lg.SetPropagate(false)
	lg.AddHandler(h)
	t.Cleanup(func() { lg.RemoveHandler(h); lg.SetPropagate(true); logging.ClearSessionContext() })
	a := &AIAgent{sessionID: "parent"}
	live := []map[string]any{{"role": "user", "content": "kept turn"}, {"role": "assistant", "content": "old reply"}}
	system := "sys"
	if _, _, err := a.compressContext(context.Background(), live, &system, compressContextOptions{TaskID: "default", CommitFence: NewCompressionCommitFence()}); err != nil {
		t.Fatal(err)
	}
	if live[0][dbPersistedMarker] != true || live[1][dbPersistedMarker] != nil {
		t.Fatalf("persisted stamp not mirrored onto the live twin: %v", live)
	}
	if v := os.Getenv("ICEBATH_SESSION_ID"); v != "child" {
		t.Fatalf("caller session id not rebound: %q", v)
	}
	lg.Info("after compression")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.tags) != 1 || h.tags[0] != " [child]" {
		t.Fatalf("caller log context not rebound: %q", h.tags)
	}
	if a.publishedCompressionCommitFence() != nil {
		t.Fatal("fence slot left published")
	}
}

// Agent whose owned progress timeout wrapper runs worker under idle and ceiling seconds against a real compressor
func ownedTimeoutAgent(t *testing.T, idle, ceiling float64, worker func(messages []map[string]any, fence *CompressionCommitFence) ([]map[string]any, string)) (*terminalActivitySession, *ContextCompressor, *atomic.Int32) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s := newTerminalActivitySession(t)
	s.agent.cachedSystemPrompt = "sys"
	compressor := newCompressorWith(t, 128_000, nil)
	s.agent.contextCompressor = compressor
	pinCompressionAuxTimeout(t, 0)
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{})
	prevCfg, prevCompress := loadCompressionConfig, facadeCompressContext
	loadCompressionConfig = func(context.Context) any {
		return map[string]any{"context_timeout_seconds": idle, "context_total_ceiling_seconds": ceiling}
	}
	calls := &atomic.Int32{}
	var workers sync.WaitGroup
	facadeCompressContext = func(_ context.Context, _ *AIAgent, messages []map[string]any, _ *string, o compressContextOptions) ([]map[string]any, string, error) {
		workers.Add(1)
		defer workers.Done()
		calls.Add(1)
		if o.CommitFence == nil {
			t.Error("owned wrapper passed no commit fence")
			return messages, "sys", nil
		}
		out, prompt := worker(messages, o.CommitFence)
		return out, prompt, nil
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(2 * time.Second); calls.Load() == 0 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		workers.Wait()
		loadCompressionConfig, facadeCompressContext = prevCfg, prevCompress
	})
	return s, compressor, calls
}

func TestOwnedTimeoutSkipsHungCompress(t *testing.T) {
	hang := make(chan struct{})
	s, compressor, calls := ownedTimeoutAgent(t, 0.05, 0.2, func(messages []map[string]any, fence *CompressionCommitFence) ([]map[string]any, string) {
		select {
		case <-hang:
		case <-time.After(2 * time.Second):
		}
		if !fence.BeginCommit(nil) {
			return messages, "sys"
		}
		defer fence.FinishCommit()
		return []map[string]any{{"role": "assistant", "content": "nope"}}, "sys"
	})
	original := []map[string]any{{"role": "user", "content": "stay"}}
	system := "sys"
	ctx := provider.WithPortalConversationContext(context.Background(), "owned-timeout")
	out, prompt, err := s.agent.compressContext(ctx, original, &system, compressContextOptions{})
	close(hang)
	if err != nil || !sameMessageList(out, original) || prompt != "sys" {
		t.Fatalf("out %v prompt %q err %v", out, prompt, err)
	}
	if !s.agent.lastCompressionTimedOut || calls.Load() != 1 || len(s.warnings) != 1 {
		t.Fatalf("timed out %v calls %d warnings %q", s.agent.lastCompressionTimedOut, calls.Load(), s.warnings)
	}
	if compressor.consecutiveTimeoutFailures != 1 {
		t.Fatalf("consecutive timeout failures %d", compressor.consecutiveTimeoutFailures)
	}
	cooldown := compressor.GetActiveCompressionFailureCooldown(false)
	if remaining, _ := cooldown["remaining_seconds"].(float64); cooldown == nil || remaining <= 0 {
		t.Fatalf("no failure cooldown recorded: %v", cooldown)
	}
	if desc, provenance, _ := activityRow(t, s.db, s.sessionID); desc != "context compression timed out" || provenance != string(state.ActivityProvenanceAgentCompressionTimeout) {
		t.Fatalf("activity %v %v", desc, provenance)
	}
}

func TestOwnedTotalCeilingReportsProgressAccurately(t *testing.T) {
	release := make(chan struct{})
	s, compressor, _ := ownedTimeoutAgent(t, 0.05, 0.15, func(messages []map[string]any, fence *CompressionCommitFence) ([]map[string]any, string) {
		for !fence.DeadlineExceeded() {
			fence.TouchProgress()
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		return messages, "sys"
	})
	original := []map[string]any{{"role": "user", "content": "stay"}}
	system := "sys"
	ctx := provider.WithPortalConversationContext(context.Background(), "owned-ceiling")
	out, prompt, err := s.agent.compressContext(ctx, original, &system, compressContextOptions{})
	close(release)
	if err != nil || !sameMessageList(out, original) || prompt != "sys" || len(s.warnings) == 0 {
		t.Fatalf("out %v prompt %q err %v warnings %q", out, prompt, err, s.warnings)
	}
	if w := s.warnings[len(s.warnings)-1]; !strings.Contains(w, "total ceiling") || !strings.Contains(w, "summary output was observed") || strings.Contains(w, "no output") {
		t.Fatalf("warning %q", w)
	}
	if e := compressor.lastSummaryError; e == nil || !strings.Contains(*e, "total ceiling exhausted") {
		t.Fatalf("cooldown error %v", e)
	}
}

func TestFallbackPromptResolvedLazilyOnTimeout(t *testing.T) {
	hang := make(chan struct{})
	s, _, _ := ownedTimeoutAgent(t, 0.05, 0.2, func(messages []map[string]any, fence *CompressionCommitFence) ([]map[string]any, string) {
		select {
		case <-hang:
		case <-time.After(2 * time.Second):
		}
		fence.BeginCommit(nil)
		return messages, "sys"
	})
	s.agent.cachedSystemPrompt = ""
	var builds atomic.Int32
	prevBuild := facadeBuildSystemPrompt
	facadeBuildSystemPrompt = func(context.Context, *AIAgent, *string) (string, error) {
		builds.Add(1)
		return "", errors.New("prompt rebuild boom")
	}
	t.Cleanup(func() { facadeBuildSystemPrompt = prevBuild })
	original := []map[string]any{{"role": "user", "content": "stay"}}
	system := "sys"
	ctx := provider.WithPortalConversationContext(context.Background(), "owned-lazy-prompt")
	out, prompt, err := s.agent.compressContext(ctx, original, &system, compressContextOptions{})
	close(hang)
	if err != nil || !sameMessageList(out, original) || prompt != "sys" || builds.Load() != 1 || len(s.warnings) != 1 {
		t.Fatalf("out %v prompt %q err %v builds %d warnings %q", out, prompt, err, builds.Load(), s.warnings)
	}
}

func TestCallerFenceBypassesOwnedWrapper(t *testing.T) {
	a := &AIAgent{sessionID: "s1", cachedSystemPrompt: "sys"}
	var seen *CompressionCommitFence
	prevCompress, prevRun := facadeCompressContext, facadeRunWithProgressTimeout
	facadeCompressContext = func(_ context.Context, _ *AIAgent, _ []map[string]any, _ *string, o compressContextOptions) ([]map[string]any, string, error) {
		seen = o.CommitFence
		return []map[string]any{{"role": "assistant", "content": "ok"}}, "sys", nil
	}
	facadeRunWithProgressTimeout = func(context.Context, compressionTimeoutRun) ([]map[string]any, string, error) {
		t.Error("owned wrapper must not run")
		return nil, "", nil
	}
	t.Cleanup(func() { facadeCompressContext, facadeRunWithProgressTimeout = prevCompress, prevRun })
	fence := NewCompressionCommitFence()
	system := "sys"
	ctx := provider.WithPortalConversationContext(context.Background(), "caller-fence")
	msgs, prompt, err := a.compressContext(ctx, []map[string]any{{"role": "user", "content": "x"}}, &system, compressContextOptions{CommitFence: fence})
	if err != nil || seen != fence || a.lastCompressionTimedOut || prompt != "sys" || msgs[0]["content"] != "ok" {
		t.Fatalf("fence %p want %p timed out %v msgs %v prompt %q err %v", seen, fence, a.lastCompressionTimedOut, msgs, prompt, err)
	}
}
