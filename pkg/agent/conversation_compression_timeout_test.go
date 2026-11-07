package agent

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestTimeoutOutcomeIsIsolatedBetweenOverlappingEntrypoints(t *testing.T) {
	a := &AIAgent{}
	workerMarked, mainReset := make(chan struct{}), make(chan struct{})
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resetContextCompressionTimeoutOutcome(a)
		markContextCompressionTimedOut(a)
		close(workerMarked)
		<-mainReset
		mu.Lock()
		seen["worker"] = contextCompressionTimedOut(a)
		mu.Unlock()
	}()
	<-workerMarked
	resetContextCompressionTimeoutOutcome(a)
	mu.Lock()
	seen["main"] = contextCompressionTimedOut(a)
	mu.Unlock()
	close(mainReset)
	wg.Wait()
	if !reflect.DeepEqual(seen, map[string]bool{"main": false, "worker": true}) {
		t.Fatalf("seen %v", seen)
	}
}

type expiredBeforeStartExecutor struct{ fence *CompressionCommitFence }

func (e expiredBeforeStartExecutor) Submit(ctx context.Context, fn func(context.Context) (any, error)) (*tools.PoolFuture, error) {
	past := pythread.Monotonic() - 1.0
	e.fence.deadline.Store(&past)
	return getCompressTimeoutExecutor().Submit(ctx, fn)
}

func runnerRows(content string) []map[string]any {
	return []map[string]any{{"role": "user", "content": content}}
}

func TestDeadlineBeforeWorkerStartUsesTimeoutFallback(t *testing.T) {
	fence := NewCompressionCommitFence()
	prev := compressTimeoutExecutorFor
	compressTimeoutExecutorFor = func() compressionExecutor { return expiredBeforeStartExecutor{fence} }
	t.Cleanup(func() { compressTimeoutExecutorFor = prev })
	original := runnerRows("keep-me")
	var workerCalls, fallbackCalls, timeouts int
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker: func(context.Context, *CompressionCommitFence) ([]map[string]any, string, error) {
			workerCalls++
			return nil, "", nil
		},
		Messages: original, SystemPromptFallback: func() string { fallbackCalls++; return "fallback-prompt" },
		IdleTimeoutSeconds: 1, TotalCeilingSeconds: 1, Fence: fence, NoStallFallback: true,
		OnTimeout: func(idle, waited, since float64) { timeouts++ },
	})
	if err != nil || workerCalls != 0 || fallbackCalls != 1 || !sameMessageList(msgs, original) || prompt != "fallback-prompt" || timeouts != 1 {
		t.Fatalf("err %v worker %d fallback %d prompt %q timeouts %d", err, workerCalls, fallbackCalls, prompt, timeouts)
	}
}

func TestProgressExtendsIdleBudgetUntilSuccess(t *testing.T) {
	original, compressed := runnerRows("a"), runnerRows("summarized")
	var seenFence atomic.Bool
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker: func(_ context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
			seenFence.Store(fence != nil)
			for i := 0; i < 6; i++ {
				time.Sleep(100 * time.Millisecond)
				fence.TouchProgress()
			}
			if !fence.BeginCommit(nil) {
				return original, "aborted", nil
			}
			defer fence.FinishCommit()
			return compressed, "ok-prompt", nil
		},
		Messages: original, SystemPromptFallback: func() string { return "fallback" }, IdleTimeoutSeconds: 0.5, TotalCeilingSeconds: 5,
	})
	if err != nil || !reflect.DeepEqual(msgs, compressed) || prompt != "ok-prompt" || !seenFence.Load() {
		t.Fatalf("msgs %v prompt %q err %v", msgs, prompt, err)
	}
}

func TestCommitStartedBeforeTimeoutReturnsWorkerResult(t *testing.T) {
	compressed := []map[string]any{{"role": "assistant", "content": "done"}}
	entered := make(chan struct{})
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker: func(_ context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
			if !fence.BeginCommit(nil) {
				t.Error("commit refused before the timeout")
			}
			close(entered)
			defer fence.FinishCommit()
			time.Sleep(200 * time.Millisecond)
			return compressed, "committed", nil
		},
		Messages: runnerRows("a"), SystemPromptFallback: func() string { return "fallback" }, IdleTimeoutSeconds: 0.05, TotalCeilingSeconds: 0.05,
	})
	<-entered
	if err != nil || !reflect.DeepEqual(msgs, compressed) || prompt != "committed" {
		t.Fatalf("msgs %v prompt %q err %v", msgs, prompt, err)
	}
}

func TestNeverFinishingCommitWaitsPastPreCommitCeilingAndReportsOverrun(t *testing.T) {
	for _, panicking := range []bool{false, true} {
		compressed := []map[string]any{{"role": "assistant", "content": "late-commit"}}
		release := make(chan struct{})
		var overruns atomic.Int32
		go func() { time.Sleep(300 * time.Millisecond); close(release) }()
		started := time.Now()
		msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
			Worker: func(_ context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
				fence.BeginCommit(nil)
				defer fence.FinishCommit()
				<-release
				return compressed, "committed-late", nil
			},
			Messages: runnerRows("a"), SystemPromptFallback: func() string { return "fallback" }, IdleTimeoutSeconds: 0.05, TotalCeilingSeconds: 0.05,
			OnCommitOverrun: func(waited, ceiling float64) {
				overruns.Add(1)
				if panicking {
					panic("overrun callback exploded")
				}
			},
		})
		if err != nil || !reflect.DeepEqual(msgs, compressed) || prompt != "committed-late" || time.Since(started) < 250*time.Millisecond || overruns.Load() != 1 {
			t.Fatalf("panicking %v msgs %v prompt %q err %v overruns %d", panicking, msgs, prompt, err, overruns.Load())
		}
	}
}

func TestRejectsNonPositiveIdle(t *testing.T) {
	_, _, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{Messages: runnerRows("a"), IdleTimeoutSeconds: 0, TotalCeilingSeconds: 1})
	if err == nil || !strings.Contains(err.Error(), "idle_timeout_seconds > 0") {
		t.Fatalf("err %v", err)
	}
}

func TestPoolSaturationRefusesNewCompression(t *testing.T) {
	compressAdmissionLock.Lock()
	prev := compressAdmittedCount
	compressAdmittedCount = compressExecutorMaxWorkers
	compressAdmissionLock.Unlock()
	t.Cleanup(func() { compressAdmissionLock.Lock(); compressAdmittedCount = prev; compressAdmissionLock.Unlock() })
	original := runnerRows("a")
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker: func(context.Context, *CompressionCommitFence) ([]map[string]any, string, error) {
			t.Error("worker ran")
			return nil, "", nil
		},
		Messages: original, SystemPromptFallback: func() string { return "fb" }, IdleTimeoutSeconds: 1, TotalCeilingSeconds: 1,
	})
	if err != nil || !sameMessageList(msgs, original) || prompt != "fb" {
		t.Fatalf("msgs %v prompt %q err %v", msgs, prompt, err)
	}
}

func TestCommitFenceCancellationAndLockRelease(t *testing.T) {
	f := NewCompressionCommitFence()
	ev := &pythread.Event{}
	if !f.CancelBeforeCommit(ev) || !ev.IsSet() || !f.IsCancelled() || f.BeginCommit(nil) {
		t.Fatal("a pre commit cancel must win and refuse admission")
	}
	g := NewCompressionCommitFence()
	if !g.BeginCommit(nil) || !g.CommitInFlight() {
		t.Fatal("commit not admitted")
	}
	if won := g.TryCancelBeforeCommit(); won != nil {
		t.Fatalf("try cancel during a commit returned %v", *won)
	}
	g.FinishCommit()
	if g.CommitInFlight() || g.CancelBeforeCommit(nil) {
		t.Fatal("cancel after an admitted commit must report the commit")
	}
	h := NewCompressionCommitFence()
	var released atomic.Int32
	h.ReleaseCancelledCompressionLock()
	if !h.RegisterCancelledLockRelease(&cancelledLockRelease{fn: func() { released.Add(1) }}) || released.Load() != 1 {
		t.Fatal("a release requested before publication must run on publication")
	}
	k := NewCompressionCommitFence()
	rel := &cancelledLockRelease{fn: func() { released.Add(1) }}
	k.RegisterCancelledLockRelease(rel)
	k.RetainCompressionLockUntilWorkerDone()
	k.ReleaseCancelledCompressionLock()
	if released.Load() != 1 {
		t.Fatal("a retained lease was released")
	}
	k.AllowCancelledLockRelease()
	k.RevokeCommitAdmission()
	if released.Load() != 2 || k.BeginCommit(nil) || k.BeginLockSetup() {
		t.Fatal("revoked admission must release once and refuse commit and lock setup")
	}
	k.ClearCancelledLockRelease(rel)
	if k.cancelledLockRelease != nil {
		t.Fatal("release not cleared")
	}
	d := NewCompressionCommitFence()
	if d.TryExtendDeadline(5) {
		t.Fatal("extended an unarmed deadline")
	}
	if err := d.SetTotalCeilingSeconds(0); err == nil {
		t.Fatal("non positive ceiling accepted")
	}
	_ = d.SetTotalCeilingSeconds(10)
	before, _ := d.DeadlineMonotonic()
	if !d.TryExtendDeadline(5) {
		t.Fatal("live deadline not extended")
	}
	if after, _ := d.DeadlineMonotonic(); after-before < 4.999 {
		t.Fatalf("deadline moved %v", after-before)
	}
	d.CancelBeforeCommit(nil)
	if d.TryExtendDeadline(5) {
		t.Fatal("a cancelled attempt was revived")
	}
}

func TestCompactionProgressStatusClassification(t *testing.T) {
	for _, s := range RoutineCompressionStatusSamples() {
		if s != CompactionDoneStatus && !IsCompactionProgressStatus(s) {
			t.Errorf("routine line not progress %q", s)
		}
	}
	for _, s := range []any{CompactionDoneStatus, ContextOverflowBlockedWarning(120000, 100000, "cooldown:30s"), "", nil, "hello"} {
		if IsCompactionProgressStatus(s) {
			t.Errorf("classified as progress %q", s)
		}
	}
	if PreAPICompressionStatus(123456) != "📦 Pre-API compression: ~123,456 tokens near the context/output limit. Compacting before the next model call." {
		t.Fatal(PreAPICompressionStatus(123456))
	}
}

func TestRequestExceedsModelWindowAndTransientBlock(t *testing.T) {
	a := &AIAgent{}
	if requestExceedsModelWindow(a, 10) != nil {
		t.Fatal("no compressor must be unknown")
	}
	c := newCompressorWith(t, 1000, nil)
	a.contextCompressor = c
	if r := requestExceedsModelWindow(a, 1001); r == nil || !*r {
		t.Fatal("over window not detected")
	}
	if r := requestExceedsModelWindow(a, 1000); r == nil || *r {
		t.Fatal("at window flagged")
	}
	markCompressionBlockedTransient(a, c, "ineffective")
	if compressionBlockedTransiently(a) {
		t.Fatal("permanent breaker marked transient")
	}
	markCompressionBlockedTransient(a, c, "cooldown:42s")
	if !compressionBlockedTransiently(a) || a.compressionBlockedTransient != "cooldown:42s" {
		t.Fatal("cooldown not marked transient")
	}
	a.compressionSkippedDueToLock = "pid=1:holder"
	if !compressionSkippedDueToLock(a) {
		t.Fatal("holder string not a lock skip")
	}
}

func TestStaleAttemptRestoreIsSkipped(t *testing.T) {
	c := newCompressorWith(t, 100000, nil)
	summary := "primary"
	c.previousSummary = &summary
	primary := claimCompressorAttempt(c)
	snap := snapshotCompressorAttemptState(c)
	fallback := "fallback"
	c.previousSummary = &fallback
	claimCompressorAttempt(c)
	if err := restoreCompressorAttemptState(c, snap, nil, nil, &primary); err != nil || c.previousSummary == nil || *c.previousSummary != "fallback" {
		t.Fatalf("stale restore clobbered the fallback state %v", err)
	}
	current := currentAttemptGeneration(c)
	if err := restoreCompressorAttemptState(c, snap, nil, nil, &current); err != nil || c.previousSummary == nil || *c.previousSummary != "primary" {
		t.Fatalf("owning restore did not apply %v", err)
	}
}

func TestCompactionDoneEdgeAndMarkerStrippedComparison(t *testing.T) {
	var got [][2]string
	a := &AIAgent{}
	emitCompactionDone(a)
	a.statusCallback = func(kind, text string) { got = append(got, [2]string{kind, text}) }
	emitCompactionDone(a)
	a.statusCallback = func(kind, text string) { panic("callback exploded") }
	emitCompactionDone(a)
	if len(got) != 1 || got[0] != [2]string{"compacted", CompactionDoneStatus} {
		t.Fatalf("emitted %v", got)
	}
	live := []map[string]any{{"role": "user", "content": "x", "_db_persisted": true}}
	swept := []map[string]any{{"role": "user", "content": "x"}}
	if !reflect.DeepEqual(stripMarkerForComparison(live), swept) || live[0]["_db_persisted"] != true {
		t.Fatalf("stripped %v live %v", stripMarkerForComparison(live), live)
	}
}

func TestTimeoutStateFirstUseIsAtomic(t *testing.T) {
	a := &AIAgent{}
	resetFinished, markerFinished := make(chan struct{}), make(chan struct{})
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resetContextCompressionTimeoutOutcome(a)
		close(resetFinished)
		<-markerFinished
		mu.Lock()
		seen["resetter"] = contextCompressionTimedOut(a)
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		markContextCompressionTimedOut(a)
		close(markerFinished)
		<-resetFinished
		mu.Lock()
		seen["marker"] = contextCompressionTimedOut(a)
		mu.Unlock()
	}()
	wg.Wait()
	if !reflect.DeepEqual(seen, map[string]bool{"resetter": false, "marker": true}) {
		t.Fatalf("seen %v", seen)
	}
}

func TestSilentWorkerTimesOutAndPreservesMessages(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	provider.InstallPluginManagerHooks(provider.PluginManagerHooks{AuxiliaryTasks: func() []map[string]any { return nil }})
	original := runnerRows("keep-me")
	started, release := make(chan struct{}), make(chan struct{})
	var commitAttempted atomic.Bool
	var warnings [][3]float64
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker: func(_ context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
			close(started)
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				t.Error("worker never released")
			}
			if !fence.BeginCommit(nil) {
				return runnerRows("should-not-land"), "x", nil
			}
			defer fence.FinishCommit()
			commitAttempted.Store(true)
			return runnerRows("too-late"), "x", nil
		},
		Messages: original, SystemPromptFallback: func() string { return "fallback-prompt" },
		IdleTimeoutSeconds: 0.05, TotalCeilingSeconds: 2,
		OnTimeout: func(idle, waited, since float64) { warnings = append(warnings, [3]float64{idle, waited, since}) },
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started")
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !commitAttempted.Load(); {
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !sameMessageList(msgs, original) || prompt != "fallback-prompt" {
		t.Fatalf("msgs %v prompt %q err %v", msgs, prompt, err)
	}
	if len(warnings) == 0 {
		t.Fatal("timeout callback should fire")
	}
	if commitAttempted.Load() {
		t.Fatal("cancelled fence must block late session mutation")
	}
}

// Worker committing one user row and prompt p
func committingWorker(seen func(ctx context.Context)) compressionWorker {
	return func(ctx context.Context, fence *CompressionCommitFence) ([]map[string]any, string, error) {
		seen(ctx)
		if !fence.BeginCommit(nil) {
			return nil, "", nil
		}
		defer fence.FinishCommit()
		return runnerRows("ok"), "p", nil
	}
}

func TestPropagatesConversationContextIntoWorker(t *testing.T) {
	var seen atomic.Value
	ctx := provider.WithPortalConversationContext(context.Background(), "conv-timeout-ctx")
	msgs, prompt, err := runCompressContextWithProgressTimeout(ctx, compressionTimeoutRun{
		Worker:   committingWorker(func(ctx context.Context) { seen.Store(provider.PortalConversationContext(ctx)) }),
		Messages: runnerRows("x"), SystemPromptFallback: func() string { return "fallback" }, IdleTimeoutSeconds: 1, TotalCeilingSeconds: 2,
	})
	if seen.Load() != "conv-timeout-ctx" || err != nil || prompt != "p" || msgs[0]["content"] != "ok" {
		t.Fatalf("ctx %v msgs %v prompt %q err %v", seen.Load(), msgs, prompt, err)
	}
}

func TestRunsWorkerOffCallerThread(t *testing.T) {
	caller := pythread.GoroutineID()
	var worker atomic.Int64
	msgs, prompt, err := runCompressContextWithProgressTimeout(context.Background(), compressionTimeoutRun{
		Worker:   committingWorker(func(context.Context) { worker.Store(int64(pythread.GoroutineID())) }),
		Messages: nil, SystemPromptFallback: func() string { return "" }, IdleTimeoutSeconds: 1, TotalCeilingSeconds: 1,
	})
	if worker.Load() == 0 || worker.Load() == int64(caller) || err != nil || prompt != "p" || msgs[0]["content"] != "ok" {
		t.Fatalf("worker %d caller %d msgs %v prompt %q err %v", worker.Load(), caller, msgs, prompt, err)
	}
}
