package whatsapp

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Bridge build, a paired session and the adapter on the port, the Python bridge dir fixture
func portReleaseSetup(t *testing.T, port int) *WhatsAppAdapter {
	t.Helper()
	hermeticHome(t)
	root := t.TempDir()
	script := filepath.Join(root, "icebath-bridge")
	if err := os.WriteFile(script, []byte("current bridge build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "session")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return newBareAdapter(t, script, session, port)
}

func TestPortReleasedBeforeSpawnSlowExitingBridgeIsEscalatedAndPortIsFreeAtSpawn(t *testing.T) {
	fastGrace(t)
	noSleep(t)
	port := freePort(t)
	holder := spawnHolder(t, true, port, t.TempDir())
	waitPortHeld(t, port, true)
	a := portReleaseSetup(t, port)
	spawns, freeAtSpawn := 0, false
	fakeSpawn(t, func(args bridgeArgs, logFH *os.File, env map[string]string) bridgeProc {
		spawns++
		freeAtSpawn = portIsFree(port)
		return exitedProc(1)
	})
	if _, err := a.Connect(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if code := a.FatalErrorCode(); code != nil && *code == "whatsapp_bridge_port_busy" {
		t.Fatal("connect gave up on a port the reap freed")
	}
	if spawns != 1 {
		t.Fatalf("the new bridge was not spawned exactly once: %d", spawns)
	}
	if !freeAtSpawn {
		t.Fatal("spawned while the port was still held")
	}
	code, dead := holder.Wait(5 * time.Second)
	if !dead {
		t.Fatal("the SIGTERM-ignoring holder was never reaped")
	}
	if code != -int(syscall.SIGKILL) {
		t.Fatalf("expected SIGKILL escalation, got %d", code)
	}
}

func TestPortReleasedBeforeSpawnStrangerHoldingPortIsLeftAloneAndNoSpawn(t *testing.T) {
	fastGrace(t)
	noSleep(t)
	port := freePort(t)
	stranger := spawnHolder(t, false, port, "")
	waitPortHeld(t, port, true)
	a := portReleaseSetup(t, port)
	spawns := 0
	fakeSpawn(t, func(args bridgeArgs, logFH *os.File, env map[string]string) bridgeProc {
		spawns++
		return &fakeProc{}
	})
	ok, err := a.Connect(context.Background(), false)
	if err != nil || ok {
		t.Fatalf("connect = %v, %v", ok, err)
	}
	if code := a.FatalErrorCode(); code == nil || *code != "whatsapp_bridge_port_busy" {
		t.Fatalf("fatal code = %v", code)
	}
	if !a.FatalErrorRetryable() {
		t.Fatal("port busy must stay retryable")
	}
	if msg := a.FatalErrorMessage(); msg == nil || !strings.Contains(*msg, strconv.Itoa(port)) {
		t.Fatalf("fatal message lacks the port: %v", msg)
	}
	if spawns != 0 {
		t.Fatal("a bridge was spawned onto a port that never came free")
	}
	if _, dead := stranger.Poll(); dead {
		t.Fatal("a non-node stranger must never be signalled")
	}
}

func TestListenerDiscoveryTimeWaitFromOldBridgeDoesNotBlockRespawn(t *testing.T) {
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := srv.Addr().(*net.TCPAddr).Port
	cli, err := net.Dial("tcp4", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := srv.Accept()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	srv.Close()
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := cli.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("server FIN not seen: %d %v", n, err)
	}
	cli.Close()
	a := newBareAdapter(t, "", t.TempDir(), port)
	if !a.waitBridgePortFree(context.Background()) {
		t.Fatal("TIME-WAIT leftovers blocked the respawn")
	}
	if a.FatalErrorCode() != nil {
		t.Fatalf("fatal set: %v", *a.FatalErrorCode())
	}
}

func TestListenerDiscoveryPsutilFallbackWhenLsofAndSsMissing(t *testing.T) {
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())
	pids, err := listenerPidsOnPort(srv.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pids, os.Getpid()) {
		t.Fatalf("kernel scan missed this process: %v", pids)
	}
}

func TestPidfileHygieneSpawnDeathUnlinksPidfile(t *testing.T) {
	noSleep(t)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	probed := make(chan struct{}, 1)
	go func() {
		if c, err := probe.Accept(); err == nil {
			probed <- struct{}{}
			c.Close()
		}
	}()
	session := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(session, 0o755); err != nil {
		t.Fatal(err)
	}
	a := newBareAdapter(t, "", session, probe.Addr().(*net.TCPAddr).Port)
	writeBridgePidfile(session, 4190000, nil)
	a.bridgeProcess = exitedProc(1)
	if a.waitForBridge(context.Background()) {
		t.Fatal("a dead bridge reported ready")
	}
	select {
	case <-probed:
		t.Fatal("must not probe a dead process")
	default:
	}
	if _, err := os.Stat(filepath.Join(session, "bridge.pid")); err == nil {
		t.Fatal("bridge.pid names a dead PID")
	}
}

func TestPidfileHygienePidfileReapEscalatesToSigkill(t *testing.T) {
	fastGrace(t)
	dir := t.TempDir()
	port := freePort(t)
	holder := spawnHolder(t, true, port, dir)
	waitPortHeld(t, port, true)
	writeBridgePidfile(dir, holder.Pid(), nil)
	if start := gateway.GetProcessStartTime(holder.Pid()); start == nil {
		t.Fatal("no start time for the holder")
	}
	killStaleBridgeByPidfile(dir)
	code, dead := holder.Wait(5 * time.Second)
	if !dead {
		t.Fatal("the SIGTERM-ignoring bridge was never reaped")
	}
	if code != -int(syscall.SIGKILL) {
		t.Fatalf("expected SIGKILL, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "bridge.pid")); err == nil {
		t.Fatal("bridge.pid survived the reap")
	}
}

func TestDisconnectWaitsDisconnectForceKillWaitsForExit(t *testing.T) {
	hermeticHome(t)
	noSleep(t)
	dir := t.TempDir()
	port := freePort(t)
	holder := spawnHolder(t, true, port, dir)
	waitPortHeld(t, port, true)
	a := newBareAdapter(t, "", dir, port)
	a.bridgeProcess = holder
	if err := a.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, dead := holder.Poll()
	if !dead {
		t.Fatal("disconnect returned before the force-killed bridge exited")
	}
	if code != -int(syscall.SIGKILL) {
		t.Fatalf("expected SIGKILL, got %d", code)
	}
}
