package mcp

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type reapCall struct {
	group   bool
	id, sig int
}

// Swaps the reaper primitives for recorders and returns the recorded signal calls
func fakeReaper(t *testing.T, exists bool, start func(int) (int64, bool), pgrp func() (int, bool)) *[]reapCall {
	t.Helper()
	prev := reaper
	calls := &[]reapCall{}
	var mu sync.Mutex
	reaper.killpg = func(pgid, sig int) error {
		mu.Lock()
		*calls = append(*calls, reapCall{true, pgid, sig})
		mu.Unlock()
		return nil
	}
	reaper.kill = func(pid, sig int) {
		mu.Lock()
		*calls = append(*calls, reapCall{false, pid, sig})
		mu.Unlock()
	}
	reaper.pidExists = func(int) bool { return exists }
	reaper.sleep = func(time.Duration) {}
	if start != nil {
		reaper.startTime = start
	}
	if pgrp != nil {
		reaper.getpgrp = pgrp
	}
	t.Cleanup(func() { reaper = prev })
	return calls
}

func (c reapCall) in(calls []reapCall) bool {
	for _, x := range calls {
		if x == c {
			return true
		}
	}
	return false
}

func addOrphan(pid int, owner string, pgid int, start int64) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	orphanStdioPIDs[pid] = true
	if owner != "" {
		orphanStdioPIDServers[pid] = owner
	}
	if pgid != 0 {
		stdioPGIDs[pid] = pgid
	}
	if start != 0 {
		stdioStartTimes[pid] = start
	}
}

func TestStabilityLoopExceptionHandlerSuppressesClosedLoop(t *testing.T) {
	logs := captureLogs(t)
	loopExceptionHandler(errLoopClosed)
	if logs.count("Unhandled exception") != 0 {
		t.Fatalf("closed loop error reached the default handler: %s", logs.text())
	}
	loopExceptionHandler(errors.New("boom"))
	if logs.count("Unhandled exception in MCP loop task: boom") != 1 {
		t.Fatalf("other errors must still log: %s", logs.text())
	}
}

func TestStabilityProbeCleanupKeepsLoopWithRegisteredServers(t *testing.T) {
	isolateHome(t)
	startTestLoop(t)
	loop := runningLoop()
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "live"}] = newServerTask("live", nil)
	mcpMu.Unlock()
	if stopMCPLoopIfIdle() {
		t.Fatal("idle stop tore down a loop that live servers use")
	}
	if runningLoop() != loop || loop == nil {
		t.Fatal("loop replaced or cleared")
	}
	if !loop.spawn(context.Background(), func(context.Context) {}) {
		t.Fatal("loop stopped accepting work")
	}
}

// Child spawned from a goroutine pinned to another OS thread still lands in the stdio ledger
func TestStabilityLedgerSeesChildSpawnedFromAnotherThread(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s := mockStdioTask(t, "threaded", map[string]any{})
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		errc <- s.start(context.Background(), s.rendered)
	}()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	defer s.shutdown(context.Background())
	s.mu.Lock()
	pids := append([]int(nil), s.stdioChildPIDs...)
	s.mu.Unlock()
	if len(pids) != 1 {
		t.Fatalf("child pids %v", pids)
	}
	mcpMu.Lock()
	owner := stdioPIDs[pids[0]]
	mcpMu.Unlock()
	if owner != "threaded" || !pidExists(pids[0]) {
		t.Fatalf("child %d not tracked (owner %q)", pids[0], owner)
	}
}

// Signal call a tracked group gets, the per pid kill where process groups do not exist
func expectTerm(pid, pgid int, force bool) reapCall {
	if groupSignalSupported {
		return reapCall{true, pgid, signalNumber(force)}
	}
	return reapCall{false, pid, signalNumber(force)}
}

func TestStabilityKillOrphanedHandlesDeadPIDs(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	addOrphan(999999999, "orphan", 0, 0)
	killOrphanedMCPChildren(false, "")
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if orphanStdioPIDs[999999999] {
		t.Fatal("dead pid kept in the orphan ledger")
	}
}

func TestStabilityRunStdioReapsOrphansBeforeSpawn(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	calls := fakeReaper(t, false, nil, nil)
	addOrphan(999999997, "", 0, 0)
	s := newServerTask("test-zombie-reap", nil)
	s.shutdownEv.Set()
	_, _ = s.runStdio(context.Background(), &Rendered{Config: map[string]any{"command": "echo", "args": []any{"hello"}}})
	mcpMu.Lock()
	left := orphanStdioPIDs[999999997]
	mcpMu.Unlock()
	if left || !(reapCall{false, 999999997, signalNumber(false)}).in(*calls) {
		t.Fatalf("orphan not reaped before spawn: left=%v calls=%v", left, *calls)
	}
}

func TestStabilityKillOrphanedFiltersByServerName(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, false, nil, nil)
	addOrphan(454545, "feishu", 0, 0)
	addOrphan(464646, "mimir", 0, 0)
	killOrphanedMCPChildren(false, "feishu")
	if len(*calls) != 1 || (*calls)[0] != (reapCall{false, 454545, signalNumber(false)}) {
		t.Fatalf("calls %v", *calls)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if orphanStdioPIDs[454545] || orphanStdioPIDServers[454545] != "" || !orphanStdioPIDs[464646] || orphanStdioPIDServers[464646] != "mimir" {
		t.Fatalf("ledger %v %v", orphanStdioPIDs, orphanStdioPIDServers)
	}
}

func TestStabilityKillOrphanedSkipsRecycledPID(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, true, func(int) (int64, bool) { return 222222, true }, nil)
	addOrphan(454545, "", 454545, 111111)
	killOrphanedMCPChildren(false, "")
	if len(*calls) != 0 {
		t.Fatalf("recycled pid signalled: %v", *calls)
	}
}

func TestStabilityKillOrphanedSignalsWhenStartTimeMatches(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, false, func(int) (int64, bool) { return 333333, true }, nil)
	addOrphan(464646, "", 464646, 333333)
	killOrphanedMCPChildren(false, "")
	if !expectTerm(464646, 464646, false).in(*calls) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestStabilityKillOrphanedWithoutBaselineKeepsLegacyBehaviour(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, false, func(int) (int64, bool) { t.Error("start time read without a baseline"); return 0, false }, nil)
	addOrphan(474747, "", 474747, 0)
	killOrphanedMCPChildren(false, "")
	if !expectTerm(474747, 474747, false).in(*calls) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestStabilityKillpgUsedWhenPgidTracked(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, true, nil, nil)
	addOrphan(525252, "", 525252, 0)
	killOrphanedMCPChildren(false, "")
	want := []reapCall{expectTerm(525252, 525252, false), expectTerm(525252, 525252, true)}
	if fmt.Sprint(*calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v", *calls)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if _, ok := stdioPGIDs[525252]; ok || orphanStdioPIDs[525252] {
		t.Fatal("ledgers kept the reaped pid")
	}
}

func TestStabilityKillpgSkippedForOwnProcessGroup(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, true, nil, func() (int, bool) { return 424242, true })
	addOrphan(717171, "", 424242, 0)
	addOrphan(818181, "", 818181, 0)
	logs := captureLogs(t)
	killOrphanedMCPChildren(false, "")
	for _, c := range *calls {
		if c.group && c.id == 424242 {
			t.Fatalf("killpg hit our own group: %v", *calls)
		}
	}
	for _, c := range []reapCall{{false, 717171, signalNumber(false)}, {false, 717171, signalNumber(true)}, expectTerm(818181, 818181, false)} {
		if !c.in(*calls) {
			t.Fatalf("missing %v in %v", c, *calls)
		}
	}
	if groupSignalSupported && logs.count("matches gateway pgid") == 0 {
		t.Fatalf("self group guard not logged: %s", logs.text())
	}
}

func TestStabilityNoPgidUsesPerPIDKill(t *testing.T) {
	isolateHome(t)
	calls := fakeReaper(t, false, nil, nil)
	addOrphan(747474, "", 0, 0)
	killOrphanedMCPChildren(false, "")
	if !(reapCall{false, 747474, signalNumber(false)}).in(*calls) {
		t.Fatalf("calls %v", *calls)
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if orphanStdioPIDs[747474] {
		t.Fatal("pid kept")
	}
}

func TestStabilityInitialConnectRetryRespectsShutdown(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	serverSleep = func(ctx context.Context, d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
			return ctx.Err() == nil
		}
	}
	var late atomic.Bool
	release := make(chan struct{})
	s, _ := runnerTask(t, "test-shutdown", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			<-release
			return "", errors.New("transient failure")
		}
		late.Store(true)
		return "", errors.New("Should not attempt after shutdown")
	})
	done := make(chan error, 1)
	go func() { done <- s.start(context.Background(), s.rendered) }()
	time.Sleep(20 * time.Millisecond)
	s.shutdownEv.Set()
	close(release)
	err := <-done
	waitUntil(t, "run did not finish", func() bool { return taskDone(s) })
	s.mu.Lock()
	published := s.err
	s.mu.Unlock()
	if err == nil || published == nil || late.Load() {
		t.Fatalf("start err %v, published %v, attempted after shutdown %v", err, published, late.Load())
	}
}

func TestStabilityPendingTaskCleanupRunsBeforeStop(t *testing.T) {
	isolateHome(t)
	ensureMCPLoop()
	l := runningLoop()
	started, cleanupRan, helperRefused := make(chan struct{}), atomic.Bool{}, atomic.Bool{}
	l.spawn(l.ctx, func(ctx context.Context) {
		close(started)
		defer func() {
			helperRefused.Store(!l.spawn(ctx, func(context.Context) {}))
			cleanupRan.Store(true)
		}()
		<-ctx.Done()
	})
	<-started
	if !stopMCPLoop(false) {
		t.Fatal("stop refused")
	}
	if !cleanupRan.Load() || !helperRefused.Load() {
		t.Fatalf("cleanup ran %v, closed loop refused new work %v", cleanupRan.Load(), helperRefused.Load())
	}
	if runningLoop() != nil || l.spawn(context.Background(), func(context.Context) {}) {
		t.Fatal("loop still accepts work after stop")
	}
}

func TestStabilityDrainBoundedWhenTaskIgnoresCancellation(t *testing.T) {
	isolateHome(t)
	ensureMCPLoop()
	l := runningLoop()
	mcpMu.Lock()
	mcpLoopState = nil
	mcpMu.Unlock()
	release, finished := make(chan struct{}), make(chan struct{})
	started := make(chan struct{})
	l.spawn(l.ctx, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release
		close(finished)
	})
	<-started
	logs := captureLogs(t)
	t0 := time.Now()
	drainMCPLoopTasks(l, 10*time.Millisecond)
	if time.Since(t0) > 500*time.Millisecond {
		t.Fatal("drain waited for the resistant task")
	}
	select {
	case <-finished:
		t.Fatal("resistant task finished before release")
	default:
	}
	close(release)
	<-finished
	if logs.count("still pending after") == 0 {
		t.Fatalf("no pending warning: %s", logs.text())
	}
}
