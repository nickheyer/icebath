package gateway

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestResolveShutdownWatchdogDelayAddsGrace(t *testing.T) {
	if got := ResolveShutdownWatchdogDelay(180, nil); got != 180+DefaultShutdownWatchdogGraceS {
		t.Fatal(got)
	}
	if got := ResolveShutdownWatchdogDelay(0, nil); got != DefaultShutdownWatchdogGraceS {
		t.Fatal(got)
	}
	if got := ResolveShutdownWatchdogDelay("bad", nil); got != DefaultShutdownWatchdogGraceS {
		t.Fatal(got)
	}
	if got := ResolveShutdownWatchdogDelay(10, 5); got != 15.0 {
		t.Fatal(got)
	}
}

func TestArmShutdownWatchdogFiresWithDumpAndExit(t *testing.T) {
	statusHome(t)
	dump := filepath.Join(t.TempDir(), "logs", "watchdog.log")
	var mu sync.Mutex
	var exitCodes []int
	snapshotCalls := 0
	fired := &pythread.Event{}
	patch(t, &watchdogOSExit, func(code int) {
		mu.Lock()
		exitCodes = append(exitCodes, code)
		mu.Unlock()
		fired.Set()
	})
	code := 9
	ArmShutdownWatchdog(context.Background(), 0.15, ShutdownWatchdogOptions{
		Done: &pythread.Event{},
		SnapshotFn: func() (any, error) {
			mu.Lock()
			snapshotCalls++
			mu.Unlock()
			return rec("active_agents", int64(1), "draining", true), nil
		},
		DumpPath: dump,
		ExitCode: &code,
	})
	if !fired.Wait(5.0) {
		t.Fatal("watchdog did not fire")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(exitCodes) != 1 || exitCodes[0] != 9 || snapshotCalls != 1 {
		t.Fatalf("exits %v snapshots %d", exitCodes, snapshotCalls)
	}
	data, err := os.ReadFile(dump)
	if err != nil || !strings.Contains(string(data), "shutdown_watchdog_fired") || !strings.Contains(string(data), "faulthandler dump") {
		t.Fatalf("%v %s", err, data)
	}
	if filepath.Base(GetShutdownWatchdogDumpPath(context.Background(), t.TempDir())) != "gateway-shutdown-watchdog.log" {
		t.Fatal("dump path name")
	}
}

func TestArmShutdownWatchdogDisarmedByDone(t *testing.T) {
	patch(t, &watchdogOSExit, func(int) { t.Error("disarmed watchdog fired") })
	done := ArmShutdownWatchdog(context.Background(), 0.2, ShutdownWatchdogOptions{DumpPath: filepath.Join(t.TempDir(), "w.log")})
	done.Set()
	time.Sleep(400 * time.Millisecond)
}

// Runs the heartbeat task on a live loop until a payload exists, then cancels it and waits for its cleanup
func runHeartbeatUntilPayload(t *testing.T, h string) map[string]any {
	t.Helper()
	loop := NewCallbackLoop()
	ctx, cancel := context.WithCancel(context.Background())
	go loop.Run(ctx)
	done := make(chan error, 1)
	go func() { done <- LoopHeartbeatForever(ctx, loop, LoopHeartbeatOptions{IntervalS: 1.0, Home: h}) }()
	path := GetLoopHeartbeatPath(context.Background(), h)
	var payload map[string]any
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &payload) == nil && len(payload) > 0 {
			break
		}
		payload = nil
	}
	if payload != nil && payload["loop_tick_socket"] == true {
		witnessAnswers(t, h, payload)
	}
	cancel()
	<-done
	loop.Close()
	if payload == nil {
		t.Fatalf("heartbeat payload did not appear at %s", path)
	}
	return payload
}

// Pings the loop tick witness and expects the one byte answer the loop writes
func witnessAnswers(t *testing.T, h string, payload map[string]any) {
	t.Helper()
	network, addr := "unix", GetLoopTickSocketPath(context.Background(), h, nil)
	if port, ok := payload["loop_tick_tcp_port"].(float64); ok {
		network, addr = "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	}
	conn, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); n != 1 || buf[0] != '1' {
		t.Fatalf("witness answered %q %v", buf[:n], err)
	}
}

func TestLoopTickWitnessArmsOnPosix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	h, err := os.MkdirTemp("", "hsw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(h)
	payload := runHeartbeatUntilPayload(t, h)
	if payload["loop_tick_socket"] != true || payload["loop_tick_tcp_port"] != nil {
		t.Fatalf("%v", payload)
	}
	if _, err := os.Stat(GetLoopTickSocketPath(context.Background(), h, nil)); !os.IsNotExist(err) {
		t.Fatalf("tick socket node left behind: %v", err)
	}
}

func TestLoopTickWitnessArmsOverTCPOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	h, err := os.MkdirTemp("", "hsw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(h)
	logs := captureLogs(t)
	payload := runHeartbeatUntilPayload(t, h)
	port, _ := payload["loop_tick_tcp_port"].(float64)
	if payload["loop_tick_socket"] != true || port <= 0 || port > 65535 || strings.Contains(logs.String(), "Loop tick socket unavailable") {
		t.Fatalf("%v %s", payload, logs)
	}
	if matches, _ := filepath.Glob(filepath.Join(h, "state", "gateway.loop-tick.*.sock")); len(matches) != 0 {
		t.Fatalf("%v", matches)
	}
}

func TestLoopTickWitnessFallsBackToTCPWhenThePathOverflowsSunPath(t *testing.T) {
	h := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	payload := runHeartbeatUntilPayload(t, h)
	port, _ := payload["loop_tick_tcp_port"].(float64)
	if payload["loop_tick_socket"] != true || port <= 0 || port > 65535 {
		t.Fatalf("%v", payload)
	}
}

func TestLoopLivenessWatchdogFiresOnAFrozenLoopAndStaysQuietOnALiveOne(t *testing.T) {
	statusHome(t)
	fired := &pythread.Event{}
	patch(t, &watchdogOSExit, func(code int) {
		if code == GatewayServiceRestartExitCode {
			fired.Set()
		}
	})
	live := NewCallbackLoop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go live.Run(ctx)
	quiet := StartLoopLivenessWatchdog(ctx, live, 0.05, 0.5, 2, GatewayServiceRestartExitCode)
	time.Sleep(400 * time.Millisecond)
	quiet.Stop()
	quiet.Join(nil)
	if fired.IsSet() || quiet.IsAlive() {
		t.Fatal("watchdog fired on a live loop")
	}
	frozen := NewCallbackLoop()
	h := StartLoopLivenessWatchdog(ctx, frozen, 0.05, 0.05, 2, GatewayServiceRestartExitCode)
	defer func() { h.Stop(); h.Join(nil) }()
	if !fired.Wait(5.0) {
		t.Fatal("watchdog never fired on a frozen loop")
	}
}

func TestLoopLivenessWatchdogEndsWhenTheLoopCloses(t *testing.T) {
	loop := NewCallbackLoop()
	loop.Close()
	h := StartLoopLivenessWatchdog(context.Background(), loop, 0.01, 0.05, 1, 1)
	timeout := 5.0
	h.Join(&timeout)
	if h.IsAlive() {
		t.Fatal("watchdog outlived a closed loop")
	}
}

func TestLoopFloorTimerReschedulesUntilCancelled(t *testing.T) {
	loop := NewCallbackLoop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)
	h, err := ArmLoopFloorTimer(loop, 0.02)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	h.Cancel()
	h.mu.Lock()
	timer := h.timer
	h.mu.Unlock()
	if timer == nil || !h.cancelled {
		t.Fatal("floor timer never scheduled")
	}
	closed := NewCallbackLoop()
	closed.Close()
	if _, err := ArmLoopFloorTimer(closed, "bad"); err != ErrLoopClosed {
		t.Fatalf("closed loop %v", err)
	}
}

func TestProcessHomeReadersExpandLiteralTilde(t *testing.T) {
	readers := map[string]func() string{
		"status":            processHermesHome,
		"lifecycle_ledger":  func() string { return watchdogHome(context.Background(), "") },
		"shutdown_watchdog": func() string { return watchdogProcessHome(context.Background()) },
	}
	for name, reader := range readers {
		tmp := t.TempDir()
		t.Setenv("HOME", tmp)
		t.Setenv("USERPROFILE", tmp)
		t.Setenv("ICEBATH_HOME", "~/.x")
		if got := reader(); got != filepath.Join(tmp, ".x") || !filepath.IsAbs(got) {
			t.Errorf("%s: %q", name, got)
		}
		t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".abs"))
		if got := reader(); got != filepath.Join(tmp, ".abs") {
			t.Errorf("%s: %q", name, got)
		}
	}
}
