package cron

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

func waitPeriodic(timeout time.Duration, pred func() bool) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if pred() {
			return true
		}
	}
	return pred()
}

type periodicLog struct {
	mu sync.Mutex
	n  int
}

func (l *periodicLog) hit() { l.mu.Lock(); l.n++; l.mu.Unlock() }
func (l *periodicLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

func goroutinesSettleTo(before int) bool {
	return waitPeriodic(time.Second, func() bool { return runtime.NumGoroutine() <= before })
}

func TestPeriodicTwoIntervalsFireProportionallyAndCancelStopsOne(t *testing.T) {
	sched := NewPeriodicScheduler()
	var fast, slow periodicLog
	hFast := sched.Schedule(context.Background(), func(context.Context) bool { fast.hit(); return true }, 10*time.Millisecond)
	hSlow := sched.Schedule(context.Background(), func(context.Context) bool { slow.hit(); return true }, 50*time.Millisecond)
	if !waitPeriodic(3*time.Second, func() bool { return slow.count() >= 3 }) {
		t.Fatal("slow callback never reached three runs")
	}
	if fast.count() <= slow.count() {
		t.Fatalf("fast %d not above slow %d", fast.count(), slow.count())
	}
	sched.mu.Lock()
	started := sched.started
	sched.mu.Unlock()
	if !started {
		t.Fatal("scheduler loop never started")
	}
	hFast.Cancel(time.Second)
	nFast := fast.count()
	time.Sleep(100 * time.Millisecond)
	if fast.count() != nFast {
		t.Fatal("cancelled callback kept firing")
	}
	if slow.count() <= 3 {
		t.Fatal("sibling callback stopped when another was cancelled")
	}
	hSlow.Cancel(time.Second)
	before := runtime.NumGoroutine()
	sched.Schedule(context.Background(), func(context.Context) bool { return true }, 10*time.Millisecond).Cancel(time.Second)
	if !goroutinesSettleTo(before) {
		t.Fatalf("goroutines %d above %d after cancel", runtime.NumGoroutine(), before)
	}
}

func TestPeriodicPanickingCallbackIsRescheduledAndDoesNotKillSibling(t *testing.T) {
	sched := NewPeriodicScheduler()
	var boom, ok periodicLog
	h1 := sched.Schedule(context.Background(), func(context.Context) bool { boom.hit(); panic(errors.New("bad callback")) }, 10*time.Millisecond)
	h2 := sched.Schedule(context.Background(), func(context.Context) bool { ok.hit(); return true }, 10*time.Millisecond)
	if !waitPeriodic(3*time.Second, func() bool { return boom.count() >= 3 && ok.count() >= 3 }) {
		t.Fatalf("boom %d ok %d", boom.count(), ok.count())
	}
	h1.Cancel(0)
	h2.Cancel(0)
}

func TestPeriodicReturningFalseStopsCallbackAndCancelWaitJoinsInflight(t *testing.T) {
	sched := NewPeriodicScheduler()
	var calls periodicLog
	sched.Schedule(context.Background(), func(context.Context) bool { calls.hit(); return false }, 10*time.Millisecond)
	if !waitPeriodic(3*time.Second, func() bool { return calls.count() == 1 }) {
		t.Fatal("callback never ran")
	}
	time.Sleep(50 * time.Millisecond)
	if calls.count() != 1 {
		t.Fatalf("calls %d after returning false", calls.count())
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := sched.Schedule(context.Background(), func(context.Context) bool {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		return true
	}, 10*time.Millisecond)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking callback never entered")
	}
	time.AfterFunc(50*time.Millisecond, func() { close(release) })
	t0 := time.Now()
	h.Cancel(2 * time.Second)
	select {
	case <-release:
	default:
		t.Fatal("cancel returned before the in-flight run finished")
	}
	if time.Since(t0) >= 1500*time.Millisecond {
		t.Fatal("cancel waited for the full bound")
	}
}

func TestPeriodicCancelInsideOwnCallbackDoesNotWaitOnItself(t *testing.T) {
	sched := NewPeriodicScheduler()
	done := make(chan time.Duration, 1)
	var h *ScheduledHandle
	ready := make(chan struct{})
	h = sched.Schedule(context.Background(), func(context.Context) bool {
		<-ready
		t0 := time.Now()
		h.Cancel(2 * time.Second)
		done <- time.Since(t0)
		return true
	}, 10*time.Millisecond)
	close(ready)
	select {
	case d := <-done:
		if d > time.Second {
			t.Fatalf("self cancel blocked for %v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback never ran")
	}
	if !h.Cancelled() {
		t.Fatal("handle not cancelled")
	}
}

func TestPeriodicModuleLevelScheduleUsesSharedDefault(t *testing.T) {
	var hits periodicLog
	h := SchedulePeriodic(context.Background(), func(context.Context) bool { hits.hit(); return true }, 10*time.Millisecond)
	if !waitPeriodic(3*time.Second, func() bool { return hits.count() > 0 }) {
		t.Fatal("shared scheduler never ran the callback")
	}
	h.Cancel(time.Second)
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"thread":"icebath-periodic-scheduler"`) {
		t.Fatal("shared scheduler loop goroutine carries no icebath-periodic-scheduler label")
	}
	before := runtime.NumGoroutine()
	handles := make([]*ScheduledHandle, 0, 20)
	for i := 0; i < 20; i++ {
		handles = append(handles, SchedulePeriodic(context.Background(), func(context.Context) bool { return true }, 10*time.Millisecond))
	}
	for _, handle := range handles {
		handle.Cancel(time.Second)
	}
	if !goroutinesSettleTo(before) {
		t.Fatalf("goroutines %d above %d after cancelling twenty handles", runtime.NumGoroutine(), before)
	}
}

func TestPeriodicCallbackRunsUnderItsOwnWorkerLabel(t *testing.T) {
	sched := NewPeriodicScheduler()
	got := make(chan string, 1)
	sched.Schedule(context.Background(), func(ctx context.Context) bool {
		v, _ := pprof.Label(ctx, "thread")
		got <- v
		return false
	}, 10*time.Millisecond)
	select {
	case v := <-got:
		if !strings.HasPrefix(v, "icebath-periodic-callback-0x") {
			t.Fatalf("worker label %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback never ran")
	}
}

func TestPeriodicBlockedCallbackDoesNotStallDueSibling(t *testing.T) {
	saved := periodicDefault
	periodicDefault = NewPeriodicScheduler()
	t.Cleanup(func() { periodicDefault = saved })
	blockerEntered := make(chan struct{})
	releaseBlocker := make(chan struct{})
	siblingRan := make(chan struct{})
	var bOnce, sOnce sync.Once
	blocker := SchedulePeriodic(context.Background(), func(context.Context) bool {
		bOnce.Do(func() { close(blockerEntered) })
		select {
		case <-releaseBlocker:
		case <-time.After(5 * time.Second):
		}
		return false
	}, 10*time.Millisecond)
	select {
	case <-blockerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("blocker never entered")
	}
	sibling := SchedulePeriodic(context.Background(), func(context.Context) bool {
		sOnce.Do(func() { close(siblingRan) })
		return false
	}, 10*time.Millisecond)
	defer func() {
		close(releaseBlocker)
		blocker.Cancel(time.Second)
		sibling.Cancel(time.Second)
	}()
	select {
	case <-siblingRan:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked periodic callback stalled an unrelated due callback")
	}
}

func TestPeriodicWorkerStartFailureKeepsTimer(t *testing.T) {
	sched := NewPeriodicScheduler()
	var attempts atomic.Int32
	real := sched.startWorker
	sched.startWorker = func(run func()) error {
		if attempts.CompareAndSwap(0, 1) {
			return errors.New("no threads")
		}
		return real(run)
	}
	var fired periodicLog
	handle := sched.Schedule(context.Background(), func(context.Context) bool { fired.hit(); return true }, 10*time.Millisecond)
	defer handle.Cancel(time.Second)
	if !waitPeriodic(3*time.Second, func() bool { return fired.count() > 0 }) {
		t.Fatal("worker-start failure silently retired the timer")
	}
	if attempts.Load() != 1 {
		t.Fatal("the fake never intercepted the callback worker")
	}
	if handle.Cancelled() {
		t.Fatal("handle cancelled after a worker start failure")
	}
}

type periodicScopeKey struct{}

func TestPeriodicCallbackKeepsScheduleContext(t *testing.T) {
	scheduler := NewPeriodicScheduler()
	observed := make(chan any, 1)
	ctx := context.WithValue(context.Background(), periodicScopeKey{}, "served-profile")
	handle := scheduler.Schedule(ctx, func(ctx context.Context) bool {
		observed <- ctx.Value(periodicScopeKey{})
		return false
	}, 10*time.Millisecond)
	defer handle.Cancel(time.Second)
	select {
	case v := <-observed:
		if v != "served-profile" {
			t.Fatalf("periodic callback lost the context captured at schedule time, got %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("periodic callback did not run")
	}
}

type periodicOwner struct {
	handle *ScheduledHandle
	pad    [64]byte
}

func (o *periodicOwner) tick(context.Context) bool { return true }

func TestPeriodicCancelledHandleReleasesItsOwner(t *testing.T) {
	owner := &periodicOwner{}
	owner.handle = SchedulePeriodic(context.Background(), owner.tick, time.Hour)
	owner.handle.Cancel(0)
	ref := weak.Make(owner)
	owner = nil
	runtime.GC()
	runtime.GC()
	if ref.Value() != nil {
		t.Fatal("cancelled handle still pins its owner")
	}
}
