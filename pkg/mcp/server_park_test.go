package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Parked revival runner that fails until revive is set, then serves until a lifecycle event
func reviveRunner(revive *atomic.Bool, firstReady bool, failure string) func(s *serverTask, ctx context.Context, call int) (string, error) {
	return func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 && firstReady {
			fakeConnected(s, false)
			return "", errors.New("subprocess died")
		}
		if !revive.Load() {
			return "", errors.New(failure)
		}
		fakeConnected(s, true)
		select {
		case <-s.reconnect.Wait():
		case <-s.shutdownEv.Wait():
		}
		return s.finishLifecycleWait(), nil
	}
}

func TestRunLoopParksInsteadOfExitingThenRevives(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 2
	var revive atomic.Bool
	s, calls := runnerTask(t, "srv", nil, reviveRunner(&revive, true, "still down"))
	s.toolNames = []string{"srv__tool"}
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "tools not deregistered on park", func() bool { return s.parked() && len(s.registeredToolNames()) == 0 })
	if taskDone(s) {
		t.Fatal("run loop exited instead of parking")
	}
	revive.Store(true)
	before := calls.Load()
	s.reconnect.Set()
	waitUntil(t, "parked task did not re-enter transport on reconnect signal", func() bool { return calls.Load() > before })
	s.shutdown(context.Background())
	if !taskDone(s) {
		t.Fatal("run task survived shutdown")
	}
}

func TestInitialConnectBudgetParksInsteadOfExitingThenRevives(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxInitialConnectRetries = 2
	var revive atomic.Bool
	s, calls := runnerTask(t, "late", nil, reviveRunner(&revive, false, "server still booting"))
	s.toolNames = []string{"late__tool"}
	if err := s.start(context.Background(), s.rendered); err == nil {
		t.Fatal("an exhausted initial budget must surface the failure")
	}
	waitUntil(t, "initial failure never parked", s.parked)
	s.mu.Lock()
	published := s.err != nil
	s.mu.Unlock()
	if calls.Load() != 3 || len(s.registeredToolNames()) != 0 || !s.ready.IsSet() || !published || taskDone(s) {
		t.Fatalf("calls %d tools %v ready %v err %v done %v", calls.Load(), s.registeredToolNames(), s.ready.IsSet(), published, taskDone(s))
	}
	revive.Store(true)
	before := calls.Load()
	s.reconnect.Set()
	waitUntil(t, "parked server did not revive", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return calls.Load() > before && s.err == nil && s.sessionProven
	})
	s.shutdown(context.Background())
}

func TestParkRecordsReasonAndProvenSessionClearsIt(t *testing.T) {
	_, env := managerHome(t)
	s := newServerTask("x", env)
	s.mu.Lock()
	s.everConnected = true
	s.mu.Unlock()
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "x"}] = s
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(mcpServers, ServerKey{Name: "x"})
		mcpMu.Unlock()
	})
	s.shutdownEv.Set()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.park(ctx, "from parked state (permanent error)")
	s.mu.Lock()
	reason := s.parkReason
	s.mu.Unlock()
	if reason != "from parked state (permanent error)" || ServerReconnecting(ctx, "x") {
		t.Fatalf("park reason %q reconnecting %v", reason, ServerReconnecting(ctx, "x"))
	}
	s.markSessionProven()
	s.mu.Lock()
	reason = s.parkReason
	s.mu.Unlock()
	if reason != "" {
		t.Fatalf("a proven session must forget the park reason, got %q", reason)
	}
}

func TestLoopNotRunningPathClearsCooldownState(t *testing.T) {
	_, env := managerHome(t)
	ctx := context.Background()
	mcpMu.Lock()
	recordConnectFailure(ctx, "bad")
	mcpServers[ServerKey{Name: "dead"}] = newServerTask("dead", env)
	mcpMu.Unlock()
	stopMCPLoop(false)
	ShutdownMCPServers(ctx, nil, nil, time.Second)
	mcpMu.Lock()
	defer mcpMu.Unlock()
	if len(serverConnectRetryAfter) != 0 || len(serverConnectFailures) != 0 {
		t.Fatalf("cooldown state survived %v %v", serverConnectRetryAfter, serverConnectFailures)
	}
}
