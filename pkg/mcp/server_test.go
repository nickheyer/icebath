package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Shortens every run loop timing for the test and restores them afterwards
func fastServerTimings(t *testing.T) {
	t.Helper()
	pr, mr, mi, sl, sw, kg := parkedRetryInterval, maxReconnectRetries, maxInitialConnectRetries, serverSleep, shutdownWait, orphanKillGrace
	kd, kf := keepaliveDefault, keepaliveFloor
	serverSleep = func(ctx context.Context, d time.Duration) bool { return ctx.Err() == nil }
	parkedRetryInterval = time.Hour
	orphanKillGrace = 50 * time.Millisecond
	t.Cleanup(func() {
		parkedRetryInterval, maxReconnectRetries, maxInitialConnectRetries, serverSleep, shutdownWait, orphanKillGrace = pr, mr, mi, sl, sw, kg
		keepaliveDefault, keepaliveFloor = kd, kf
	})
}

// Starts the MCP loop for the test and stops it afterwards
func startTestLoop(t *testing.T) {
	t.Helper()
	ensureMCPLoop()
	t.Cleanup(func() { stopMCPLoop(false) })
}

// Polls cond until it holds or five seconds pass
func waitUntil(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Task whose transport is the given runner, already wired to a test home
func runnerTask(t *testing.T, name string, cfg map[string]any, runner func(s *serverTask, ctx context.Context, call int) (string, error)) (*serverTask, *atomic.Int64) {
	t.Helper()
	s := newServerTask(name, &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	calls := &atomic.Int64{}
	s.transportRunner = func(ctx context.Context, r *Rendered) (string, error) {
		return runner(s, ctx, int(calls.Add(1)))
	}
	if cfg == nil {
		cfg = map[string]any{"command": "fake"}
	}
	s.rendered = &Rendered{Config: cfg}
	return s, calls
}

// Live session stand in for a runner, ready and connected
func fakeConnected(s *serverTask, proven bool) {
	s.ready.Set()
	s.mu.Lock()
	s.everConnected = true
	s.sessionProven = false
	s.mu.Unlock()
	if proven {
		s.markSessionProven()
	}
}

func (s *serverTask) parked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wasParked
}

func taskDone(s *serverTask) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func TestServerShutdownWinsWhenBothSet(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	s.shutdownEv.Set()
	s.reconnect.Set()
	reason, err := s.waitForLifecycleEvent(context.Background(), nil)
	if err != nil || reason != "shutdown" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
}

func TestServerReconnectOrReraise(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	drop := &sessionDropError{err: errors.New("stream closed")}
	if _, err := s.reconnectOrReraise(context.Background(), drop); err == nil {
		t.Fatal("a drop before readiness must re-raise for backoff")
	}
	s.ready.Set()
	if reason, err := s.reconnectOrReraise(context.Background(), drop); err != nil || reason != "reconnect" {
		t.Fatalf("live session drop: reason=%q err=%v", reason, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.reconnectOrReraise(ctx, drop); err == nil {
		t.Fatal("a cancelled task must re-raise")
	}
	s.shutdownEv.Set()
	if _, err := s.reconnectOrReraise(context.Background(), drop); err == nil {
		t.Fatal("a drop during shutdown must re-raise")
	}
}

func TestServerProvenClearsBudget(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	s.reconnectRetries = 4
	s.wasParked = true
	s.parkReason = "from parked state (permanent error)"
	logs := captureLogs(t)
	s.markSessionProven()
	if !s.sessionProven || s.reconnectRetries != 0 || s.parkReason != "" || s.wasParked {
		t.Fatalf("proven=%v retries=%d reason=%q parked=%v", s.sessionProven, s.reconnectRetries, s.parkReason, s.wasParked)
	}
	if logs.count("revived") != 1 {
		t.Fatalf("revival not logged once: %s", logs.text())
	}
}

func TestServerFlappingTransportParksWithinBudget(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 3
	s, calls := runnerTask(t, "flap", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		fakeConnected(s, false)
		return "reconnect", nil
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "flapping transport never parked", s.parked)
	if n := calls.Load(); n > 5 {
		t.Fatalf("budget did not bound respawns: %d transport calls", n)
	}
	s.shutdown(context.Background())
	if !taskDone(s) {
		t.Fatal("run task survived shutdown")
	}
}

func TestServerProvenSessionDoesNotBurnBudget(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 3
	s, calls := runnerTask(t, "healthy", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		fakeConnected(s, true)
		if call > 12 {
			s.shutdownEv.Set()
			return "shutdown", nil
		}
		return "reconnect", nil
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "run loop never exited", func() bool { return taskDone(s) })
	if s.parked() || calls.Load() <= 10 {
		t.Fatalf("parked=%v calls=%d", s.parked(), calls.Load())
	}
}

func TestServerReconnectCounterResetsAfterSuccessfulSession(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 3
	s, calls := runnerTask(t, "alternating", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call > 10 {
			s.shutdownEv.Set()
			return "shutdown", nil
		}
		if call%2 == 0 {
			return "", errors.New("connection reset by peer")
		}
		fakeConnected(s, true)
		return "reconnect", nil
	})
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "run loop never exited", func() bool { return taskDone(s) })
	if s.parked() || calls.Load() < 8 {
		t.Fatalf("parked=%v calls=%d", s.parked(), calls.Load())
	}
}

func TestServerReconnectCounterStillParksOnConsecutiveFailures(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxReconnectRetries = 2
	s, _ := runnerTask(t, "dying", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			fakeConnected(s, true)
			return "reconnect", nil
		}
		return "", errors.New("connection refused")
	})
	logs := captureLogs(t)
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "consecutive failures never parked", s.parked)
	if logs.count("failed after 2 reconnection attempts, parking") != 1 {
		t.Fatalf("park line missing: %s", logs.text())
	}
	s.shutdown(context.Background())
}

func TestServerInitialConnectFailureParksAndRevives(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	maxInitialConnectRetries = 0
	var revive atomic.Bool
	s, _ := runnerTask(t, "initial-failure", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if !revive.Load() {
			return "", errors.New("backend still booting")
		}
		fakeConnected(s, true)
		<-s.reconnect.Wait()
		return s.finishLifecycleWait(), nil
	})
	err := s.start(context.Background(), s.rendered)
	if err == nil || !strings.Contains(err.Error(), "backend still booting") {
		t.Fatalf("start error: %v", err)
	}
	waitUntil(t, "initial failure never parked", s.parked)
	if taskDone(s) {
		t.Fatal("recoverable initial failure was not parked")
	}
	revive.Store(true)
	s.reconnect.Set()
	waitUntil(t, "parked server did not revive", func() bool { return s.ready.IsSet() && !s.parked() })
	s.mu.Lock()
	cleared := s.err == nil
	s.mu.Unlock()
	if !cleared {
		t.Fatal("revival kept the published initial error")
	}
	s.shutdown(context.Background())
	if !taskDone(s) {
		t.Fatal("run task survived shutdown")
	}
}

func TestServerPermanentInitialFailureParksAtOnce(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s, calls := runnerTask(t, "perm", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		return "", &HTTPStatusError{Status: 401}
	})
	logs := captureLogs(t)
	if err := s.start(context.Background(), s.rendered); err == nil {
		t.Fatal("permanent failure must reach start")
	}
	waitUntil(t, "permanent failure never parked", s.parked)
	if calls.Load() != 1 || logs.count("failed initial authentication, parking until credentials change", "icebath mcp login perm") != 1 {
		t.Fatalf("calls=%d logs=%s", calls.Load(), logs.text())
	}
	s.shutdown(context.Background())
}

// Writes config.yaml with the given mcp_servers block into the test home
func writeServersConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("mcp_servers:\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServerParkedSelfProbeRevivesWithoutBrowser(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	writeServersConfig(t, "  probe:\n    command: fake\n")
	maxInitialConnectRetries = 0
	parkedRetryInterval = 20 * time.Millisecond
	var interactiveOnProbe atomic.Int32
	s, _ := runnerTask(t, "probe", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			return "", errors.New("connection refused")
		}
		if isInteractive(ctx) {
			interactiveOnProbe.Store(1)
		} else {
			interactiveOnProbe.Store(2)
		}
		fakeConnected(s, true)
		<-s.reconnect.Wait()
		return s.finishLifecycleWait(), nil
	})
	logs := captureLogs(t)
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "self-probe never revived the parked server", func() bool { return interactiveOnProbe.Load() != 0 && !s.parked() })
	if interactiveOnProbe.Load() != 2 {
		t.Fatal("an unattended self-probe left interactive OAuth enabled")
	}
	if logs.count("revived") != 1 {
		t.Fatalf("revival not logged: %s", logs.text())
	}
	s.shutdown(context.Background())
}

func TestServerParkedProbeSkipsDisabledEntry(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	writeServersConfig(t, "  off:\n    command: fake\n    enabled: false\n")
	maxInitialConnectRetries = 0
	parkedRetryInterval = 10 * time.Millisecond
	s, calls := runnerTask(t, "off", nil, func(s *serverTask, ctx context.Context, call int) (string, error) {
		if call == 1 {
			return "", errors.New("connection refused")
		}
		fakeConnected(s, true)
		<-s.reconnect.Wait()
		return s.finishLifecycleWait(), nil
	})
	logs := captureLogs(t)
	_ = s.start(context.Background(), s.rendered)
	waitUntil(t, "pause never announced", func() bool { return logs.count("pausing the self-probe") >= 3 })
	if calls.Load() != 1 {
		t.Fatalf("self-probe rebuilt a disabled server: %d calls", calls.Load())
	}
	infos := 0
	for _, r := range logs.all() {
		if strings.Contains(r.msg, "pausing the self-probe") && r.level == slog.LevelInfo {
			infos++
		}
	}
	if infos != 1 {
		t.Fatalf("pause announced %d times at info", infos)
	}
	s.reconnect.Set()
	waitUntil(t, "explicit reconnect did not revive the disabled entry", func() bool { return calls.Load() == 2 })
	s.shutdown(context.Background())
}

func TestServerParkLogDedupe(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	logs := captureLogs(t)
	s.logPark("MCP server 'x' parked: boom")
	s.wasParked = true
	s.logPark("MCP server 'x' parked: boom")
	s.logPark("MCP server 'x' parked: boom")
	s.logPark("MCP server 'x' parked: auth")
	var levels []slog.Level
	for _, r := range logs.all() {
		levels = append(levels, r.level)
	}
	want := []slog.Level{slog.LevelWarn, slog.LevelDebug, slog.LevelDebug, slog.LevelWarn}
	if len(levels) != len(want) {
		t.Fatalf("levels %v", levels)
	}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("levels %v want %v", levels, want)
		}
	}
}

func TestServerParkAfterRevivalWarnsAgain(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	logs := captureLogs(t)
	line := "MCP server 'x' failed after 5 reconnection attempts, parking"
	s.logPark(line)
	s.wasParked = true
	s.markSessionProven()
	s.logPark(line)
	s.wasParked = true
	s.logPark(line)
	if n := len(logs.all()); n != 4 {
		t.Fatalf("records %d: %s", n, logs.text())
	}
	got := logs.all()
	if got[0].level != slog.LevelWarn || got[2].level != slog.LevelWarn || got[3].level != slog.LevelDebug {
		t.Fatalf("levels %v %v %v", got[0].level, got[2].level, got[3].level)
	}
}

func TestServerStdioChildrenDead(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	if s.stdioChildrenDead() {
		t.Fatal("no captured pids must read as unknown")
	}
	s.stdioChildPIDs = []int{os.Getpid()}
	if s.stdioChildrenDead() {
		t.Fatal("a live pid reads as dead")
	}
	s.stdioChildPIDs = []int{os.Getpid(), 1 << 30}
	if s.stdioChildrenDead() {
		t.Fatal("one live pid of two reads as dead")
	}
	s.stdioChildPIDs = []int{1 << 30}
	if !s.stdioChildrenDead() {
		t.Fatal("a vanished pid reads as alive")
	}
	s.config = map[string]any{"url": "https://example.test/mcp"}
	if s.stdioChildrenDead() {
		t.Fatal("HTTP servers never fail fast on child death")
	}
}

func TestServerStdioDeadChildBeforeProofReconnects(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	keepaliveDefault = 20 * time.Millisecond
	s := newServerTask("x", nil)
	s.stdioChildPIDs = []int{1 << 30}
	logs := captureLogs(t)
	reason, err := s.waitForLifecycleEvent(context.Background(), nil)
	if err != nil || reason != "reconnect" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	if s.sessionProven || logs.count("stdio child exited before the session proved healthy") != 1 {
		t.Fatalf("proven=%v logs=%s", s.sessionProven, logs.text())
	}
	s.stdioChildPIDs = []int{os.Getpid()}
	go func() {
		time.Sleep(60 * time.Millisecond)
		s.shutdownEv.Set()
	}()
	if reason, _ := s.waitForLifecycleEvent(context.Background(), nil); reason != "shutdown" || !s.sessionProven {
		t.Fatalf("live child idling a full interval: reason=%q proven=%v", reason, s.sessionProven)
	}
}

func TestServerStdioRecycleDeadline(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	idle := 0.02
	s.idleTimeout = &idle
	reason, err := s.waitForLifecycleEvent(context.Background(), nil)
	if err != nil || reason != "recycle" || s.recycledReason != "idle_timeout_seconds" || !s.isRecycledStdio() {
		t.Fatalf("reason=%q err=%v recycled=%q", reason, err, s.recycledReason)
	}
}

func TestServerFailInflightCallsFlagsTeardownRace(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	ctx, cancel := context.WithCancel(context.Background())
	untrack := s.trackInflight(cancel)
	s.failInflightCalls("reconnect")
	if ctx.Err() == nil || !s.teardownRace || !s.isReconnecting() || !strings.Contains(s.suspectReason, "reconnect tore down 1 in-flight call(s)") {
		t.Fatalf("race=%v suspect=%q", s.teardownRace, s.suspectReason)
	}
	untrack()
	if s.inflightCount() != 0 {
		t.Fatal("untrack left the call registered")
	}
}

func TestServerEnsureHealthyWithoutSessionRequestsReconnect(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	if !s.ensureHealthy(context.Background(), time.Second) {
		t.Fatal("an unsuspected connection must pass")
	}
	s.markSuspect("keepalive failed")
	if s.ensureHealthy(context.Background(), time.Second) || !s.reconnect.IsSet() || s.suspectReason != "" {
		t.Fatal("a suspect without a session must hand recovery to the reconnect path")
	}
}

// Real stdio task over the mock server with the given extra env
func mockStdioTask(t *testing.T, name string, env map[string]any) *serverTask {
	t.Helper()
	cfg := map[string]any{"command": mockBinary(t), "args": []any{}, "env": env}
	s := newServerTask(name, &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	s.rendered = &Rendered{Config: cfg}
	return s
}

func TestServerStdioEndToEndDrainsPagesAndReleasesChild(t *testing.T) {
	home := isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	s := mockStdioTask(t, "pages", map[string]any{"MOCK_MCP_PAGES": "3"})
	mcpMu.Lock()
	mcpServers[ServerKey{Name: "pages"}] = s
	mcpMu.Unlock()
	if err := s.start(context.Background(), s.rendered); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n, pids := len(s.tools), append([]int(nil), s.stdioChildPIDs...)
	s.mu.Unlock()
	if n != 3 || len(s.registeredToolNames()) != 3 {
		t.Fatalf("discovered %d tools, registered %v", n, s.registeredToolNames())
	}
	if len(pids) != 1 {
		t.Fatalf("child pids %v", pids)
	}
	mcpMu.Lock()
	owner := stdioPIDs[pids[0]]
	mcpMu.Unlock()
	if owner != "pages" {
		t.Fatalf("child not ledgered: %q", owner)
	}
	s.shutdown(context.Background())
	waitUntil(t, "child survived shutdown", func() bool { return !pidExists(pids[0]) })
	mcpMu.Lock()
	_, live := stdioPIDs[pids[0]]
	mcpMu.Unlock()
	if live || len(s.registeredToolNames()) != 0 {
		t.Fatal("shutdown left the child ledgered or tools registered")
	}
	data, err := os.ReadFile(filepath.Join(home, "logs", "mcp-stderr.log"))
	if err != nil || !strings.Contains(string(data), "===== starting MCP server 'pages' =====") {
		t.Fatalf("stderr log header missing: %v %q", err, data)
	}
}

func TestServerStdioWarnsOnIdentityHeaderAndMissingCommand(t *testing.T) {
	isolateHome(t)
	s := newServerTask("x", nil)
	logs := captureLogs(t)
	_, err := s.runStdio(context.Background(), &Rendered{Config: map[string]any{"identity_header": map[string]any{"name": "X"}}})
	if err == nil || err.Error() != "MCP server 'x' has no 'command' in config" {
		t.Fatalf("err %v", err)
	}
	if logs.count("identity_header is only supported on HTTP/SSE transports") != 1 {
		t.Fatalf("logs %s", logs.text())
	}
}

func TestServerOrphanSweepKillsTrackedChild(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	s := mockStdioTask(t, "orphan", map[string]any{})
	tr, err := StartStdio(context.Background(), StdioParams{Command: mockBinary(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	pid := tr.PID()
	s.trackSpawnedChildren([]int{pid})
	mcpMu.Lock()
	delete(stdioPIDs, pid)
	orphanStdioPIDs[pid] = true
	orphanStdioPIDServers[pid] = "orphan"
	mcpMu.Unlock()
	killOrphanedMCPChildren(false, "someone-else")
	if !tr.Alive() {
		t.Fatal("a sweep scoped to another server killed this child")
	}
	killOrphanedMCPChildren(false, "orphan")
	waitUntil(t, "orphan survived the sweep", func() bool { return !tr.Alive() })
	mcpMu.Lock()
	_, left := stdioPGIDs[pid]
	mcpMu.Unlock()
	if left {
		t.Fatal("swept pid kept its group ledger entry")
	}
}

func TestServerSignalSkipsRecycledPID(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)
	signalMCPProcess(os.Getpid(), false, "x", 0, false, 0, false, -1<<40, true)
	if logs.count("start-time mismatch") != 1 {
		t.Fatalf("recycled pid was not refused: %s", logs.text())
	}
}

func TestLifecycleShutdownScopedLeavesOtherProfiles(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	a, b := newServerTask("srv", nil), newServerTask("srv", nil)
	keyA, keyB := ServerKey{Scope: "/profiles/a", Name: "srv"}, ServerKey{Scope: "/profiles/b", Name: "srv"}
	mcpMu.Lock()
	mcpServers[keyA], mcpServers[keyB] = a, b
	serverScopeKeys[keyA], serverScopeKeys[keyB] = "/profiles/a", "/profiles/b"
	serverToolScopes[keyA] = map[string]bool{"/profiles/a": true, "/profiles/c": true}
	serverConnectRetryAfter[keyA] = time.Now()
	mcpMu.Unlock()
	scope := "/profiles/a"
	ShutdownMCPServers(context.Background(), &scope, nil, time.Second)
	mcpMu.Lock()
	_, hasA := mcpServers[keyA]
	_, hasB := mcpServers[keyB]
	_, cooled := serverConnectRetryAfter[keyA]
	adopters := orphanedAdopters["/profiles/c"]
	mcpMu.Unlock()
	if hasA || !hasB || cooled || !adopters["srv"] {
		t.Fatalf("hasA=%v hasB=%v cooled=%v adopters=%v", hasA, hasB, cooled, adopters)
	}
	if !a.shutdownEv.IsSet() || b.shutdownEv.IsSet() || runningLoop() == nil {
		t.Fatal("scoped shutdown touched another profile or stopped the shared loop")
	}
	reregisterOrphanedAdopters(context.Background(), nil)
	mcpMu.Lock()
	drained := len(orphanedAdopters) == 0
	mcpMu.Unlock()
	if !drained {
		t.Fatal("pending adopters were not drained")
	}
	ShutdownMCPServers(context.Background(), nil, nil, time.Second)
	if runningLoop() != nil || len(mcpServers) != 0 {
		t.Fatal("wildcard shutdown left the loop or a server")
	}
}
