package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// One listener start the runtime asked for
type listenerStartCall struct {
	host   string
	port   int
	webapp *gateway.WebappMount
}

// Runtime listener start for config with the listener itself stood in, the busy port or refusal failing the Webapp's start
func startStubbedRuntimeAPI(t *testing.T, config string, busy bool, refuse error) (*GatewayRunner, []listenerStartCall, string, error) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	var calls []listenerStartCall
	prev, prevDist := startGatewayAPIFn, gateway.WebappRendererBuild.PackagedDist
	t.Cleanup(func() { startGatewayAPIFn, gateway.WebappRendererBuild.PackagedDist = prev, prevDist })
	gateway.WebappRendererBuild.PackagedDist = func() string { return "" }
	startGatewayAPIFn = func(_ context.Context, _ *GatewayRunner, host string, port int, webapp *gateway.WebappMount) (*gatewayAPIHandle, error) {
		calls = append(calls, listenerStartCall{host, port, webapp})
		if busy && port != 0 {
			return nil, &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}
		}
		if refuse != nil && webapp != nil {
			return nil, refuse
		}
		if port == 0 {
			port = 50001
		}
		return &gatewayAPIHandle{apiOrigin: fmt.Sprintf("http://127.0.0.1:%d", port)}, nil
	}
	r := allocGatewayRunner()
	r.sessionRuntimeDescriptor.store(map[string]any{})
	server, err := gateway.NewGatewayControlServer(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sessionControlServer = server
	startErr := startGatewayRuntimeAPI(context.Background(), r)
	return r, calls, h, startErr
}

// Host and port of each listener start in call order
func listenerStartAddrs(calls []listenerStartCall) [][2]any {
	out := make([][2]any, len(calls))
	for i, call := range calls {
		out[i] = [2]any{call.host, call.port}
	}
	return out
}

func TestTheGatewayListensOnTheWebappPortWhenEnabled(t *testing.T) {
	r, calls, h, err := startStubbedRuntimeAPI(t, "webapp:\n  enabled: true\n  port: 9555\n", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].host != "127.0.0.1" || calls[0].port != 9555 {
		t.Fatalf("listener starts %+v", calls)
	}
	mount := calls[0].webapp
	if mount == nil || mount.Home != h || filepath.Base(mount.Dist) != "dist-webapp" {
		t.Fatalf("webapp mount %+v", mount)
	}
	status := r.webappListenerStatus()
	if status.State != "serving" || status.URL == nil || *status.URL != "http://127.0.0.1:9555" || status.Reason != nil {
		t.Fatalf("status %+v", status)
	}
}

func TestABusyWebappPortStillLeavesNativeClientsAListener(t *testing.T) {
	r, calls, _, err := startStubbedRuntimeAPI(t, "webapp:\n  enabled: true\n  port: 9555\n", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]any{{"127.0.0.1", 9555}, {"127.0.0.1", 0}}; !reflect.DeepEqual(listenerStartAddrs(calls), want) {
		t.Fatalf("listener starts %v", listenerStartAddrs(calls))
	}
	if calls[1].webapp != nil {
		t.Fatal("the private listener was started with the Webapp mounted")
	}
	if origin := r.SessionRuntimeDescriptor()["api_origin"]; origin != "http://127.0.0.1:50001" {
		t.Fatalf("api origin %v", origin)
	}
	status := r.webappListenerStatus()
	if status.State != "unavailable" || status.Reason == nil || !strings.Contains(*status.Reason, "127.0.0.1:9555") {
		t.Fatalf("status %+v", status)
	}
}

func TestAGatewayWithoutTheWebappStartsMainsPrivateListener(t *testing.T) {
	r, calls, _, err := startStubbedRuntimeAPI(t, "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (listenerStartCall{"127.0.0.1", 0, nil}) {
		t.Fatalf("listener starts %+v", calls)
	}
	status := r.webappListenerStatus()
	if status.State != "off" || status.URL != nil || status.Reason != nil {
		t.Fatalf("status %+v", status)
	}
}

func TestAWebappListenerThatCannotStartStillLeavesNativeClientsAListener(t *testing.T) {
	for name, refusal := range map[string]error{
		"auth_gate_exit": &gateway.DashboardExit{Code: 1, Msg: "Refusing to bind dashboard to 0.0.0.0 — the auth gate engages on non-loopback binds " +
			"(0.0.0.0), but no auth providers are registered."},
		"lifespan_failure": &gateway.PyError{Kind: "RuntimeError", Msg: "gateway API lifespan startup failed"},
	} {
		t.Run(name, func(t *testing.T) {
			r, calls, _, err := startStubbedRuntimeAPI(t, "webapp:\n  enabled: true\n  host: 0.0.0.0\n", false, refusal)
			if err != nil {
				t.Fatal(err)
			}
			if want := [][2]any{{"0.0.0.0", 9120}, {"127.0.0.1", 0}}; !reflect.DeepEqual(listenerStartAddrs(calls), want) {
				t.Fatalf("listener starts %v", listenerStartAddrs(calls))
			}
			status := r.webappListenerStatus()
			if status.State != "unavailable" || status.Reason == nil || !strings.Contains(*status.Reason, refusal.Error()) {
				t.Fatalf("status %+v", status)
			}
			if origin := r.SessionRuntimeDescriptor()["api_origin"]; origin != "http://127.0.0.1:50001" {
				t.Fatalf("api origin %v", origin)
			}
		})
	}
}

func TestCancellingTheGatewayWhileTheWebappListenerStartsCancelsIt(t *testing.T) {
	_, calls, _, err := startStubbedRuntimeAPI(t, "webapp:\n  enabled: true\n", false, context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled start returned %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("a cancelled start fell back to another listener: %+v", calls)
	}
}

func TestTheGatewayRegistersTheWebappVerbsBeforeItsControlSocketAnswers(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	var seen map[string]any
	prev := controlServerStart
	controlServerStart = func(server *gateway.GatewayControlServer) bool {
		seen = askVerb(t, server, "webapp-status", "same-user-peer", nil)
		return false
	}
	t.Cleanup(func() { controlServerStart = prev })
	r := allocGatewayRunner()
	r.sessionRuntimeDescriptor.store(map[string]any{})
	loop := gateway.NewCallbackLoop()
	t.Cleanup(loop.Close)
	if server := startGatewayStartControlSocket(context.Background(), r, loop); server != nil {
		t.Fatal("a control socket that did not bind was returned")
	}
	if seen["ok"] != true {
		t.Fatalf("webapp-status answered %v", seen)
	}
	if result, _ := seen["result"].(map[string]any); result["state"] != "starting" {
		t.Fatalf("webapp-status answered %v", seen)
	}
}

func TestTheWebappListenerIsDualStackOnlyOnTheIpv6Wildcard(t *testing.T) {
	stop := errors.New("stop")
	for _, row := range []struct {
		host      string
		dualStack bool
	}{{"::", true}, {"::1", false}, {"127.0.0.1", false}} {
		t.Run(row.host, func(t *testing.T) {
			var asked []bool
			prevSocket, prevBuild := listenerSocketFn, BuildGatewayListener
			t.Cleanup(func() { listenerSocketFn, BuildGatewayListener = prevSocket, prevBuild })
			listenerSocketFn = func(_ netip.Addr, _ int, dualStack bool) (*listenerSocket, error) {
				asked = append(asked, dualStack)
				return nil, stop
			}
			BuildGatewayListener = func(context.Context, gateway.GatewayAPIRunner, *gateway.ListenerState, *gateway.WebappMount, string, int) (gateway.GatewayListenerServer, error) {
				return &fakeListenerServer{mount: &fakeDashboardMount{}, exit: make(chan struct{})}, nil
			}
			_, err := startGatewayAPI(context.Background(), allocGatewayRunner(), row.host, 9120, &gateway.WebappMount{})
			if !errors.Is(err, stop) {
				t.Fatalf("listener start returned %v", err)
			}
			if !reflect.DeepEqual(asked, []bool{row.dualStack}) {
				t.Fatalf("dual stack asked %v, want %v", asked, row.dualStack)
			}
		})
	}
}
