package whatsapp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loopback stand-in of the bridge HTTP API, the adapter pointed at its port
func fakeBridgeServer(t *testing.T, a *WhatsAppAdapter, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a.bridgePort = srv.Listener.Addr().(*net.TCPAddr).Port
	return srv
}

// Open log file standing in for the bridge log handle
func openLog(t *testing.T) *os.File {
	t.Helper()
	fh, err := os.Create(filepath.Join(t.TempDir(), "bridge.log"))
	if err != nil {
		t.Fatal(err)
	}
	return fh
}

// Whether the file handle was closed
func isClosed(fh *os.File) bool {
	_, err := fh.Write([]byte("x"))
	return err != nil
}

// Runner exposing the signal stop flag
type signalRunner struct{ stop bool }

func (r signalRunner) StopRequestedBySignal() bool { return r.stop }

// Counts fatal handler calls
func countingFatalHandler(a *WhatsAppAdapter) *atomic.Int32 {
	n := &atomic.Int32{}
	a.SetFatalErrorHandler(func(ctx context.Context, adapter gateway.PlatformAdapter) error {
		n.Add(1)
		return nil
	})
	return n
}

func TestDataInitializedNoNameErrorWhenJsonAlwaysFails(t *testing.T) {
	noSleep(t)
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 0)
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) })
	a.bridgeProcess = &fakeProc{}
	if !a.waitForBridge(context.Background()) {
		t.Fatal("an up HTTP server with an unparseable body must warn and proceed")
	}
}

func TestFileHandleClosedOnErrorClosedWhenBridgeDiesPhase1(t *testing.T) {
	noSleep(t)
	a := portReleaseSetup(t, freePort(t))
	var logFH *os.File
	fakeSpawn(t, func(args bridgeArgs, fh *os.File, env map[string]string) bridgeProc {
		logFH = fh
		return exitedProc(1)
	})
	ok, err := a.Connect(context.Background(), false)
	if err != nil || ok {
		t.Fatalf("connect = %v, %v", ok, err)
	}
	if logFH == nil || !isClosed(logFH) {
		t.Fatal("bridge log handle left open")
	}
	if a.bridgeLogFH != nil {
		t.Fatal("bridge log handle still referenced")
	}
}

func TestBridgeRuntimeFailureSendMarksRetryableFatalWhenManagedBridgeExits(t *testing.T) {
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 19876)
	calls := countingFatalHandler(a)
	a.MarkConnected(context.Background(), "")
	a.httpSession = &http.Client{}
	fh := openLog(t)
	a.bridgeLogFH = fh
	a.bridgeProcess = exitedProc(7)
	result, err := a.Send(context.Background(), "chat-123", "hello", "", nil)
	if err != nil || result.Success {
		t.Fatalf("send = %+v, %v", result, err)
	}
	if code := a.FatalErrorCode(); code == nil || *code != "whatsapp_bridge_exited" {
		t.Fatalf("fatal code = %v", code)
	}
	if !a.FatalErrorRetryable() {
		t.Fatal("bridge exit must be retryable")
	}
	if eventually(2*time.Second, func() bool { return calls.Load() == 1 }) != nil {
		t.Fatalf("fatal handler calls = %d", calls.Load())
	}
	if !isClosed(fh) || a.bridgeLogFH != nil {
		t.Fatal("bridge log handle left open")
	}
}

func TestBridgeRuntimeFailureSignalExitDuringGatewaySignalShutdownIsNotFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"sigterm", -15}, {"sigint", -2}} {
		t.Run(tc.name, func(t *testing.T) {
			a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 19876)
			calls := countingFatalHandler(a)
			a.MarkConnected(context.Background(), "")
			a.shuttingDown = false
			a.GatewayRunner = signalRunner{stop: true}
			a.bridgeLogFH = openLog(t)
			a.bridgeProcess = exitedProc(tc.code)
			if msg := a.checkManagedBridgeExit(context.Background()); msg != "" {
				t.Fatalf("shutdown exit reported as %q", msg)
			}
			if a.FatalErrorCode() != nil {
				t.Fatal("fatal set on a shutdown exit")
			}
			time.Sleep(50 * time.Millisecond)
			if calls.Load() != 0 {
				t.Fatal("fatal handler ran on a shutdown exit")
			}
		})
	}
}

func TestBridgeRuntimeFailureSigtermExitWithoutGatewayShutdownStaysFatal(t *testing.T) {
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 19876)
	calls := countingFatalHandler(a)
	a.MarkConnected(context.Background(), "")
	a.GatewayRunner = signalRunner{stop: false}
	a.bridgeLogFH = openLog(t)
	a.bridgeProcess = exitedProc(-15)
	if msg := a.checkManagedBridgeExit(context.Background()); msg == "" {
		t.Fatal("a SIGTERM while the gateway runs must stay fatal")
	}
	if code := a.FatalErrorCode(); code == nil || *code != "whatsapp_bridge_exited" || !a.FatalErrorRetryable() {
		t.Fatalf("fatal = %v retryable=%v", code, a.FatalErrorRetryable())
	}
	if eventually(2*time.Second, func() bool { return calls.Load() == 1 }) != nil {
		t.Fatalf("fatal handler calls = %d", calls.Load())
	}
}

func TestBridgeRuntimeFailureSendNormalizesBarePhoneNumbersToJid(t *testing.T) {
	hermeticHome(t)
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 0)
	var mu sync.Mutex
	var posted []map[string]any
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		if r.URL.Path == "/send" {
			posted = append(posted, body)
		}
		mu.Unlock()
		w.Write([]byte(`{"success":true,"messageId":"msg-1"}`))
	})
	a.MarkConnected(context.Background(), "")
	a.httpSession = &http.Client{}
	result, err := a.Send(context.Background(), "+15551234567", "hello", "", nil)
	if err != nil || !result.Success {
		t.Fatalf("send = %+v, %v", result, err)
	}
	if len(posted) != 1 || posted[0]["chatId"] != "15551234567@s.whatsapp.net" || posted[0]["message"] != "hello" || len(posted[0]) != 2 {
		t.Fatalf("posted = %v", posted)
	}
}

func TestBridgeRuntimeFailureClosedWhenBridgeDiesPhase2(t *testing.T) {
	noSleep(t)
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", 0)
	fakeBridgeServer(t, a, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"disconnected"}`)) })
	polls := 0
	a.bridgeProcess = &fakeProc{poll: func() (int, bool) {
		polls++
		return 1, polls > 15
	}}
	fh := openLog(t)
	a.bridgeLogFH = fh
	if a.waitForBridge(context.Background()) {
		t.Fatal("a bridge dead in phase 2 reported ready")
	}
	if !isClosed(fh) || a.bridgeLogFH != nil {
		t.Fatal("bridge log handle left open")
	}
}

// Records commands and answers netstat with the given output
func fakeCommands(t *testing.T, netstat string) *[][]string {
	t.Helper()
	var calls [][]string
	saved := runCommand
	runCommand = func(timeout time.Duration, name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if name == "netstat" {
			return netstat, nil
		}
		return "", nil
	}
	t.Cleanup(func() { runCommand = saved })
	return &calls
}

// Forces the node bridge identity verdict
func fakeIdentity(t *testing.T, verdict bool) {
	t.Helper()
	saved := pidLooksLikeNodeBridge
	pidLooksLikeNodeBridge = func(int) bool { return verdict }
	t.Cleanup(func() { pidLooksLikeNodeBridge = saved })
}

// Selects the Windows branch
func onWindows(t *testing.T, on bool) {
	t.Helper()
	saved := isWindows
	isWindows = on
	t.Cleanup(func() { isWindows = saved })
}

// Records signals instead of sending them
func recordKills(t *testing.T) *[][2]int {
	t.Helper()
	var kills [][2]int
	saved := signalPid
	signalPid = func(pid int, sig syscall.Signal) error {
		kills = append(kills, [2]int{pid, int(sig)})
		return nil
	}
	t.Cleanup(func() { signalPid = saved })
	return &kills
}

const windowsNetstat = "  Proto  Local Address          Foreign Address        State           PID\n" +
	"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       12345\n" +
	"  TCP    0.0.0.0:3001           0.0.0.0:0              LISTENING       99999\n"

func TestKillPortProcessUsesNetstatAndTaskkillOnWindows(t *testing.T) {
	onWindows(t, true)
	fakeIdentity(t, true)
	calls := fakeCommands(t, windowsNetstat)
	killPortProcess(3000)
	sawNetstat, sawKill := false, false
	for _, c := range *calls {
		sawNetstat = sawNetstat || c[0] == "netstat"
		sawKill = sawKill || (len(c) == 4 && c[0] == "taskkill" && c[1] == "/PID" && c[2] == "12345" && c[3] == "/F")
	}
	if !sawNetstat || !sawKill {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestKillPortProcessWindowsRefusesTaskkillOnNonBridgePid(t *testing.T) {
	onWindows(t, true)
	fakeIdentity(t, false)
	calls := fakeCommands(t, windowsNetstat[:len(windowsNetstat)-len("  TCP    0.0.0.0:3001           0.0.0.0:0              LISTENING       99999\n")])
	killPortProcess(3000)
	for _, c := range *calls {
		if c[0] == "taskkill" {
			t.Fatalf("taskkill fired on a non-bridge pid: %v", *calls)
		}
	}
}

func TestKillPortProcessKillsOnlyListenersOnLinux(t *testing.T) {
	onWindows(t, false)
	fakeIdentity(t, true)
	kills := recordKills(t)
	var asked []int
	saved := listenerPidsOnPort
	listenerPidsOnPort = func(port int) ([]int, error) {
		asked = append(asked, port)
		return []int{55555}, nil
	}
	t.Cleanup(func() { listenerPidsOnPort = saved })
	saveGrace := sigtermGrace
	sigtermGrace = 0
	t.Cleanup(func() { sigtermGrace = saveGrace })
	killPortProcess(3000)
	if len(asked) != 1 || asked[0] != 3000 {
		t.Fatalf("listener lookup = %v", asked)
	}
	if len(*kills) != 1 || (*kills)[0] != [2]int{55555, int(syscall.SIGTERM)} {
		t.Fatalf("kills = %v", *kills)
	}
}

func TestKillPortProcessNonBridgeListenerIsNeverKilled(t *testing.T) {
	onWindows(t, false)
	fakeIdentity(t, false)
	kills := recordKills(t)
	saved := listenerPidsOnPort
	listenerPidsOnPort = func(port int) ([]int, error) { return []int{55555}, nil }
	t.Cleanup(func() { listenerPidsOnPort = saved })
	killPortProcess(3000)
	if len(*kills) != 0 {
		t.Fatalf("kills = %v", *kills)
	}
}

// Transport counting idle connection closes, the aiohttp session close of the Python case
type closeCountingTransport struct {
	http.RoundTripper
	closes int
}

func (c *closeCountingTransport) CloseIdleConnections() { c.closes++ }

func TestHttpSessionLifecycleSessionClosedOnDisconnect(t *testing.T) {
	hermeticHome(t)
	a := newBareAdapter(t, "/tmp/test-bridge", t.TempDir(), 19876)
	transport := &closeCountingTransport{RoundTripper: http.DefaultTransport}
	a.httpSession = &http.Client{Transport: transport}
	a.MarkConnected(context.Background(), "")
	if err := a.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.closes != 1 {
		t.Fatalf("session closes = %d", transport.closes)
	}
	if a.httpSession != nil {
		t.Fatal("session still referenced")
	}
}

func TestNoCredsPreflightConnectProceedsWhenCredsPresent(t *testing.T) {
	noSleep(t)
	a := portReleaseSetup(t, freePort(t))
	fakeSpawn(t, func(args bridgeArgs, fh *os.File, env map[string]string) bridgeProc { return exitedProc(1) })
	ok, err := a.Connect(context.Background(), false)
	if err != nil || ok {
		t.Fatalf("connect = %v, %v", ok, err)
	}
	if code := a.FatalErrorCode(); code != nil && *code == "whatsapp_not_paired" {
		t.Fatal("paired session failed the preflight")
	}
}

func TestNamedConnectFailuresBridgeHttpTimeoutIsANamedRetryableFailure(t *testing.T) {
	noSleep(t)
	a := newBareAdapter(t, "/tmp/test-bridge", "/tmp/test-wa-session", freePort(t))
	a.bridgeProcess = &fakeProc{}
	if a.waitForBridge(context.Background()) {
		t.Fatal("an HTTP server that never came up reported ready")
	}
	if code := a.FatalErrorCode(); code == nil || *code != "whatsapp_bridge_timeout" || !a.FatalErrorRetryable() {
		t.Fatalf("fatal = %v retryable=%v", code, a.FatalErrorRetryable())
	}
}
