package whatsapp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Adapter of one profile home built inside that profile's scope
func profileAdapter(t *testing.T, h string, extra map[string]any) (*WhatsAppAdapter, context.Context) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), h)
	if extra == nil {
		extra = map[string]any{}
	}
	a, err := NewWhatsAppAdapter(ctx, &gateway.PlatformConfig{Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	flushStatus(t)
	return a, ctx
}

// Bridge build and paired session so the preflight passes
func passPreflight(t *testing.T, a *WhatsAppAdapter) {
	t.Helper()
	if err := os.MkdirAll(a.sessionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.sessionPath, "creds.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "icebath-bridge")
	if err := os.WriteFile(script, []byte("bridge build"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.bridgeScript = script
}

// Loopback listener holding a free port
func holdPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// Fails the case when any bridge reap or listener scan runs
func forbidKills(t *testing.T) {
	t.Helper()
	savedIdentity, savedSignal := pidLooksLikeNodeBridge, signalPid
	pidLooksLikeNodeBridge = func(pid int) bool { t.Errorf("identity checked for pid %d", pid); return false }
	signalPid = func(pid int, sig syscall.Signal) error { t.Errorf("signal %v sent to %d", sig, pid); return nil }
	t.Cleanup(func() { pidLooksLikeNodeBridge, signalPid = savedIdentity, savedSignal })
}

func TestSecondaryPortsAndForeignListener(t *testing.T) {
	hermeticHome(t)
	noSleep(t)
	root := t.TempDir()
	var adapters []*WhatsAppAdapter
	var held net.Listener
	for _, name := range []string{"a", "b"} {
		h := filepath.Join(root, name)
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
		a, ctx := profileAdapter(t, h, nil)
		a.Traits.RuntimeStatusPlatformKey = name + ":whatsapp"
		if ok, _ := a.Connect(ctx, false); ok {
			t.Fatal("an unpaired profile connected")
		}
		adapters = append(adapters, a)
		if name == "a" {
			ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(a.bridgePort))
			if err != nil {
				t.Fatal(err)
			}
			held = ln
		}
	}
	if p := adapters[0].bridgePort; p < 3001 || p > 3999 {
		t.Fatalf("secondary port %d outside 3001..3999", p)
	}
	explicit, ctx := profileAdapter(t, filepath.Join(root, "b"), map[string]any{"bridge_port": adapters[0].bridgePort})
	explicit.Traits.RuntimeStatusPlatformKey = "b:whatsapp"
	passPreflight(t, explicit)
	forbidKills(t)
	probed := false
	go func() {
		if c, err := held.Accept(); err == nil {
			probed = true
			c.Close()
		}
	}()
	if ok, _ := explicit.Connect(ctx, false); ok {
		t.Fatal("connected onto a foreign listener")
	}
	msg := explicit.FatalErrorMessage()
	if !explicit.HasFatalError() || explicit.FatalErrorRetryable() || msg == nil ||
		!strings.Contains(*msg, strconv.Itoa(explicit.bridgePort)) || !strings.Contains(*msg, "platforms.whatsapp.extra.bridge_port") {
		t.Fatalf("fatal = %v retryable=%v", msg, explicit.FatalErrorRetryable())
	}
	time.Sleep(50 * time.Millisecond)
	if probed {
		t.Fatal("must not probe foreign bridge")
	}
	_ = explicit.Disconnect(ctx)
	held.Close()
	if adapters[0].bridgePort == adapters[1].bridgePort {
		t.Fatal("both profiles share a port")
	}
	for i, name := range []string{"a", "b"} {
		raw, err := os.ReadFile(filepath.Join(root, name, "platforms", "whatsapp", "bridge_port"))
		if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(adapters[i].bridgePort) {
			t.Fatalf("record of %s = %q %v", name, raw, err)
		}
		restart, rctx := profileAdapter(t, filepath.Join(root, name), nil)
		restart.Traits.RuntimeStatusPlatformKey = name + ":whatsapp"
		_, _ = restart.Connect(rctx, false)
		if restart.bridgePort != adapters[i].bridgePort {
			t.Fatalf("restart of %s moved to %d", name, restart.bridgePort)
		}
	}
}

func TestTimeWaitLeftoversAreNotAForeignListener(t *testing.T) {
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := srv.Addr().(*net.TCPAddr).Port
	cli, err := net.Dial("tcp4", srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := srv.Accept()
	if err != nil {
		t.Fatal(err)
	}
	accepted.Close()
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = cli.Read(make([]byte, 1))
	cli.Close()
	srv.Close()
	session := t.TempDir()
	if !portIsFree(port) {
		t.Fatal("TIME-WAIT read as busy")
	}
	if verdict, err := CheckSecondaryOwnership(session, port); err != nil || verdict != "free" {
		t.Fatalf("verdict = %q %v", verdict, err)
	}
	sock, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	if portIsFree(port) {
		t.Fatal("a live listener read as free")
	}
	if _, err := CheckSecondaryOwnership(session, port); err == nil || !strings.Contains(err.Error(), "does not own") {
		t.Fatalf("err = %v", err)
	}
}

func TestAllocationSkipsPortsRecordedBySiblings(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles")
	sibling, fresh := filepath.Join(profiles, "a"), filepath.Join(profiles, "b")
	recorded := 0
	for p := 3001; p < 4000; p++ {
		if portIsFree(p) {
			recorded = p
			break
		}
	}
	if err := os.MkdirAll(filepath.Join(sibling, "platforms", "whatsapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "platforms", "whatsapp", "bridge_port"), []byte(strconv.Itoa(recorded)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if p, err := SecondaryBridgePort(fresh, nil); err != nil || p == recorded {
		t.Fatalf("fresh profile took %d %v", p, err)
	}
	if p, err := SecondaryBridgePort(sibling, nil); err != nil || p != recorded {
		t.Fatalf("sibling = %d %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "platforms", "whatsapp", "bridge_port"), []byte("30o1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SecondaryBridgePort(fresh, nil); err == nil || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("err = %v", err)
	}
}

func TestOwnershipVerdicts(t *testing.T) {
	session := t.TempDir()
	pidFile := filepath.Join(session, "bridge.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	sock, port := holdPort(t)
	owns := func() bool {
		_, err := CheckSecondaryOwnership(session, port)
		return err == nil
	}
	if owns() {
		t.Fatal("legacy pidfile proved ownership")
	}
	writeBridgePidfile(session, os.Getpid(), &port)
	if v, err := CheckSecondaryOwnership(session, port); err != nil || v != "ours" {
		t.Fatalf("verdict = %q %v", v, err)
	}
	other := port + 1
	writeBridgePidfile(session, os.Getpid(), &other)
	if owns() {
		t.Fatal("a bridge on another port proved ownership")
	}
	writeBridgePidfile(session, os.Getpid(), nil)
	if owns() {
		t.Fatal("a pre-port pidfile proved ownership")
	}
	start, _ := gateway.GetProcessStartTime(os.Getpid()).(int64)
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n%d\n%d\n", os.Getpid(), start+1, port)), 0o644); err != nil {
		t.Fatal(err)
	}
	if owns() {
		t.Fatal("a fingerprint mismatch proved ownership")
	}
	sock.Close()
	if v, err := CheckSecondaryOwnership(session, port); err != nil || v != "free" {
		t.Fatalf("verdict = %q %v", v, err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatal("pidfile was unlinked")
	}
}

func TestSecondaryAdoptsItsOwnOrphanedBridge(t *testing.T) {
	hermeticHome(t)
	h := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	a, ctx := profileAdapter(t, h, nil)
	a.Traits.RuntimeStatusPlatformKey = "c:whatsapp"
	passPreflight(t, a)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprintf(w, `{"status":"connected","scriptHash":%q,"sendReadReceipts":false,"session":%q}`, fileContentHash(a.bridgeScript), a.sessionPath)
			return
		}
		io.WriteString(w, "[]")
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := os.MkdirAll(filepath.Join(h, "platforms", "whatsapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "platforms", "whatsapp", "bridge_port"), []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBridgePidfile(a.sessionPath, os.Getpid(), &port)
	saved := listenerPidsOnPort
	listenerPidsOnPort = func(int) ([]int, error) { t.Error("a secondary never clears a port by scan"); return nil, nil }
	t.Cleanup(func() { listenerPidsOnPort = saved })
	ok, err := a.Connect(ctx, false)
	if err != nil || !ok {
		t.Fatalf("connect = %v %v", ok, err)
	}
	defer a.Disconnect(ctx)
	if a.HasFatalError() {
		t.Fatal("fatal set on adoption")
	}
	if a.bridgeProcess != nil {
		t.Fatal("an adopted bridge is not managed")
	}
}

func TestLiveOldBridgeDoesNotAuthorizeADifferentPort(t *testing.T) {
	hermeticHome(t)
	oldSock, oldPort := holdPort(t)
	defer oldSock.Close()
	foreignSock, foreignPort := holdPort(t)
	defer foreignSock.Close()
	h := filepath.Join(t.TempDir(), "d")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	a, ctx := profileAdapter(t, h, map[string]any{"bridge_port": foreignPort})
	a.Traits.RuntimeStatusPlatformKey = "d:whatsapp"
	passPreflight(t, a)
	writeBridgePidfile(a.sessionPath, os.Getpid(), &oldPort)
	forbidKills(t)
	probed := false
	go func() {
		if c, err := foreignSock.Accept(); err == nil {
			probed = true
			c.Close()
		}
	}()
	if ok, _ := a.Connect(ctx, false); ok {
		t.Fatal("connected through a foreign endpoint")
	}
	msg := a.FatalErrorMessage()
	if !a.HasFatalError() || a.FatalErrorRetryable() || msg == nil || !strings.Contains(*msg, strconv.Itoa(foreignPort)) {
		t.Fatalf("fatal = %v", msg)
	}
	time.Sleep(50 * time.Millisecond)
	if probed {
		t.Fatal("must not probe or adopt a foreign endpoint")
	}
	_ = a.Disconnect(ctx)
}

func TestLaunchStartupAndStandaloneSendAgreeAfterRoleChange(t *testing.T) {
	hermeticHome(t)
	root := filepath.Join(t.TempDir(), "profiles")
	h := filepath.Join(root, "e")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	recorded, err := SecondaryBridgePort(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	launch, ctx := profileAdapter(t, h, nil)
	_, _ = launch.Connect(ctx, false)
	if standalone, _ := StandaloneBridgePort(h, nil); launch.bridgePort != recorded || standalone != recorded {
		t.Fatalf("launch %d, recorded %d, standalone %d", launch.bridgePort, recorded, standalone)
	}
	fresh := filepath.Join(root, "f")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	defaultLike, fctx := profileAdapter(t, fresh, nil)
	_, _ = defaultLike.Connect(fctx, false)
	if standalone, _ := StandaloneBridgePort(fresh, nil); defaultLike.bridgePort != 3000 || standalone != 3000 {
		t.Fatalf("default-like %d, standalone %d", defaultLike.bridgePort, standalone)
	}
}
