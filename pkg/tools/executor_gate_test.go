package tools

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const gateSession = "test-session-79719"

// Clears the human wait table before and after a test like the Python autouse fixture
func cleanHumanWait(t *testing.T) {
	clear := func() {
		humanWait.Lock()
		humanWait.states = map[string]*humanWaitState{}
		humanWait.order = nil
		humanWait.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func makeGate(env *Env, lock *time.Duration) *AuthorizationGate {
	key := gateSession
	return NewAuthorizationGateWith(env, AuthorizationGateOptions{LockTimeout: lock, SessionKey: &key})
}

func gateEnv(t *testing.T) *Env {
	cleanHumanWait(t)
	return approvalTestEnv(t, gateSession, map[string]any{"mode": "manual", "timeout": 300}, nil)
}

func TestHumanWaitOpenWindowCounts(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, gateSession)
	time.Sleep(50 * time.Millisecond)
	if HumanWaitSeconds(env, gateSession) <= 0 {
		t.Fatal("open window not counted")
	}
	release()
	first := HumanWaitSeconds(env, gateSession)
	time.Sleep(50 * time.Millisecond)
	if HumanWaitSeconds(env, gateSession) != first {
		t.Fatal("closed window kept growing")
	}
}

func TestHumanWaitOverlappingWindowsCoalesce(t *testing.T) {
	env := gateEnv(t)
	start := time.Now()
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := humanWaitWindow(env, gateSession)
			<-gate
			release()
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	if got := HumanWaitSeconds(env, gateSession); got > time.Since(start).Seconds()+0.05 {
		t.Fatalf("double counted %.3f", got)
	}
}

func TestHumanWaitSessionsAreIsolated(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, "other-session")
	time.Sleep(50 * time.Millisecond)
	if HumanWaitSeconds(env, gateSession) != 0 {
		t.Fatal("other session leaked")
	}
	release()
	if HumanWaitSeconds(env, "other-session") <= 0 {
		t.Fatal("other session not recorded")
	}
}

func backdateWindow(key string) {
	humanWait.Lock()
	humanWait.states[key].windowStarted = time.Now().Add(-24 * time.Hour)
	humanWait.Unlock()
}

func TestHumanWaitOpenWindowClampedToApprovalTimeout(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, gateSession)
	defer release()
	backdateWindow(gateSession)
	if got := HumanWaitSeconds(env, gateSession); got > 300+60 {
		t.Fatalf("unclamped %.1f", got)
	}
}

func TestHumanWaitEvictionKeepsPendingSessions(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, gateSession)
	defer release()
	for i := range humanWaitMaxSessions + 8 {
		humanWaitWindow(env, "burst-"+string(rune('a'+i%26))+time.Duration(i).String())()
	}
	humanWait.Lock()
	defer humanWait.Unlock()
	st, ok := humanWait.states[gateSession]
	if !ok || st.pending != 1 || len(humanWait.states) > humanWaitMaxSessions {
		t.Fatalf("pending session evicted %v %d", ok, len(humanWait.states))
	}
}

func TestHumanWaitLateCloseOfWedgedWindowIsClamped(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, gateSession)
	backdateWindow(gateSession)
	release()
	if got := HumanWaitSeconds(env, gateSession); got > 300+60 {
		t.Fatalf("late close injected %.1f", got)
	}
}

func TestAuthorizationGateSerializesCallbacks(t *testing.T) {
	g := makeGate(gateEnv(t), nil)
	var mu sync.Mutex
	active, maxActive := 0, 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Run(func() {
				mu.Lock()
				active++
				maxActive = max(maxActive, active)
				mu.Unlock()
				time.Sleep(30 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("max active %d", maxActive)
	}
}

func TestAuthorizationGateLockTimeoutDegradesToUnserialized(t *testing.T) {
	bound := 100 * time.Millisecond
	g := makeGate(gateEnv(t), &bound)
	holderIn, release := make(chan struct{}), make(chan struct{})
	go g.Run(func() { close(holderIn); <-release })
	<-holderIn
	start := time.Now()
	value := ""
	g.Run(func() { value = "ran-unserialized" })
	close(release)
	if value != "ran-unserialized" || time.Since(start) >= 2*time.Second {
		t.Fatalf("starved %q %v", value, time.Since(start))
	}
}

func wedgeGate(g *AuthorizationGate) func() {
	in, release := make(chan struct{}), make(chan struct{})
	go g.Run(func() { close(in); <-release })
	<-in
	return func() { close(release) }
}

func TestAuthorizationGateWedgedCallbackContributesNothingToExclusion(t *testing.T) {
	g := makeGate(gateEnv(t), nil)
	defer wedgeGate(g)()
	time.Sleep(150 * time.Millisecond)
	if g.ExcludedSeconds() != 0 {
		t.Fatalf("residency excluded %.3f", g.ExcludedSeconds())
	}
}

func TestAuthorizationGateDeadlineArithmeticConvergesWithWedgedWorker(t *testing.T) {
	g := makeGate(gateEnv(t), nil)
	defer wedgeGate(g)()
	deadline := time.Now().Add(300 * time.Millisecond)
	remaining := func() float64 { return time.Until(deadline).Seconds() + g.ExcludedSeconds() }
	first := remaining()
	time.Sleep(150 * time.Millisecond)
	if second := remaining(); second >= first {
		t.Fatal("remaining is constant")
	}
	time.Sleep(250 * time.Millisecond)
	if remaining() > 0 {
		t.Fatal("deadline never became due")
	}
}

func TestAuthorizationGateHumanWaitIsExcluded(t *testing.T) {
	env := gateEnv(t)
	g := makeGate(env, nil)
	release := humanWaitWindow(env, gateSession)
	time.Sleep(100 * time.Millisecond)
	release()
	if g.ExcludedSeconds() < 0.09 {
		t.Fatalf("excluded %.3f", g.ExcludedSeconds())
	}
}

func TestAuthorizationGateBaselineIgnoresWaitsBeforeBatch(t *testing.T) {
	env := gateEnv(t)
	release := humanWaitWindow(env, gateSession)
	time.Sleep(100 * time.Millisecond)
	release()
	if g := makeGate(env, nil); g.ExcludedSeconds() != 0 {
		t.Fatalf("pre batch wait excluded %.3f", g.ExcludedSeconds())
	}
}

func TestAuthorizationGateOtherSessionsWaitNotExcluded(t *testing.T) {
	env := gateEnv(t)
	g := makeGate(env, nil)
	release := humanWaitWindow(env, "unrelated-session")
	time.Sleep(50 * time.Millisecond)
	release()
	if g.ExcludedSeconds() != 0 {
		t.Fatalf("unrelated wait excluded %.3f", g.ExcludedSeconds())
	}
}

// Executor over a fake host recording activity with the heartbeat at 50 ms
func heartbeatExecutor(t *testing.T) (*Executor, *fakeExecutorHost) {
	old := toolActivityHeartbeatInterval
	toolActivityHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { toolActivityHeartbeatInterval = old })
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	return NewExecutor(host, NewDispatcher(NewRegistry())), host
}

func (h *fakeExecutorHost) touches(prefix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, a := range h.activity {
		if len(a) >= len(prefix) && a[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestHeartbeatTouchesPeriodicallyAndStops(t *testing.T) {
	e, host := heartbeatExecutor(t)
	_, _ = e.runWithActivityHeartbeat(context.Background(), "terminal", func() (string, error) {
		for host.touches("tool running: terminal") < 2 {
			time.Sleep(10 * time.Millisecond)
		}
		return "", nil
	})
	n := host.touches("")
	time.Sleep(150 * time.Millisecond)
	if host.touches("") != n || host.touches("tool running: terminal") != n {
		t.Fatalf("heartbeat kept running or stamped another label")
	}
}

func runTerminalManaged(e *Executor, execute func(context.Context, map[string]any) (string, error)) (*ManagedToolResult, error) {
	return e.runToolExecutionMiddleware(context.Background(), managedInput{name: "terminal", args: map[string]any{"command": "true"},
		taskID: "task", toolCallID: "tc1", execute: execute, displayIndex: 1})
}

func TestSlowToolCallRefreshesActivityDuringExecution(t *testing.T) {
	e, host := heartbeatExecutor(t)
	managed, err := runTerminalManaged(e, func(context.Context, map[string]any) (string, error) {
		time.Sleep(250 * time.Millisecond)
		return `{"ok": true}`, nil
	})
	if err != nil || managed.Result != `{"ok": true}` || host.touches("tool running: terminal") < 3 {
		t.Fatalf("err %v result %q touches %d", err, managed.Result, host.touches("tool running: terminal"))
	}
}

func TestFastToolCallDoesNotLeaveStrayHeartbeat(t *testing.T) {
	e, host := heartbeatExecutor(t)
	_, _ = runTerminalManaged(e, func(context.Context, map[string]any) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "ok", nil
	})
	n := host.touches("")
	time.Sleep(120 * time.Millisecond)
	if host.touches("") != n {
		t.Fatal("heartbeat kept running after the tool returned")
	}
}

func TestHeartbeatStopsWhenExecuteRaises(t *testing.T) {
	e, host := heartbeatExecutor(t)
	boom := errors.New("tool exploded")
	if _, err := runTerminalManaged(e, func(context.Context, map[string]any) (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Fatalf("error lost %v", err)
	}
	n := host.touches("")
	time.Sleep(120 * time.Millisecond)
	if host.touches("") != n {
		t.Fatal("heartbeat leaked after a raising tool")
	}
}

func TestDispatchBindsActivityCallbackAndProgressRelay(t *testing.T) {
	e, host := heartbeatExecutor(t)
	var events []string
	host.progressCb = func(event, _, _ string, _ map[string]any, _ map[string]any) { events = append(events, event) }
	_, err := runTerminalManaged(e, func(ctx context.Context, _ map[string]any) (string, error) {
		ActivityCallbackOf(ctx)("probe stamp")
		CurrentToolProgress(ctx)(1, nil, "half")
		return "ok", nil
	})
	if err != nil || host.touches("probe stamp") != 1 || len(events) != 2 || events[1] != ToolProgressEvent {
		t.Fatalf("err %v stamps %d events %v", err, host.touches("probe stamp"), events)
	}
}
