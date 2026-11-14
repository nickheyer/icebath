package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session id a dashboard token socket creates, after a wrong token is refused at the upgrade
func createWithSessionToken(t *testing.T, origin, token, requestID string, timeout time.Duration) string {
	t.Helper()
	url := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
	refused, response, err := websocket.DefaultDialer.Dial(url+"?token=wrong", nil)
	if err == nil {
		refused.Close()
		t.Fatal("a wrong token was accepted")
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("wrong token dial = %v", err)
	}
	ws, response, err := websocket.DefaultDialer.Dial(url+"?token="+token, nil)
	if err != nil {
		status := ""
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			status = fmt.Sprintf(" (%s %s)", response.Status, body)
		}
		t.Fatalf("%v%s", err, status)
	}
	defer ws.Close()
	if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session.create",
		"params": map[string]any{"request_id": requestID, "source": "cli"}}); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	for {
		var reply map[string]any
		if err := ws.ReadJSON(&reply); err != nil {
			t.Fatal(err)
		}
		if reply["id"] == float64(1) {
			result, ok := reply["result"].(map[string]any)
			if !ok {
				t.Fatalf("session.create = %v", reply)
			}
			sid, _ := result["session_id"].(string)
			return sid
		}
	}
}

// Event loop running for the rest of the case, the loop an async hermes case awaits the runner on
func runningCaseLoop(t *testing.T) *gateway.CallbackLoop {
	t.Helper()
	loop := gateway.NewCallbackLoop()
	loopCtx, stop := context.WithCancel(context.Background())
	go loop.Run(loopCtx)
	t.Cleanup(func() {
		loop.Close()
		stop()
	})
	return loop
}

// Transport with no behaviour of its own, the bare object an unbound hermes connection holds
type bareTransport struct{ tuigateway.Transport }

func TestAuthenticatedTokenCanCreateWithoutIdentitylessPermissions(t *testing.T) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "home"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", user)
	config, err := json.Marshal(map[string]any{
		"model":             map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": "http://127.0.0.1:1/v1"},
		"platform_toolsets": map[string]any{"cli": []any{}},
		"auxiliary":         map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := dashboard.ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := gateway.ProcessOwnership.Reserve(ctx, []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	defer gateway.ProcessOwnership.Close()
	runner, err := gatewayrun.NewGatewayRunner(ctx, gateway.DefaultGatewayConfig(ctx))
	if err != nil {
		t.Fatal(err)
	}
	runner.BindGatewayLoop(runningCaseLoop(t))
	var control *gateway.GatewayControlServer
	defer func() {
		if err := runner.Stop(ctx, false, false, false); err != nil {
			t.Error(err)
		}
		if control != nil {
			control.Stop()
		}
	}()
	if err := gatewayrun.InitializeGatewayRuntime(ctx, runner); err != nil {
		t.Fatal(err)
	}
	if control, err = gateway.NewGatewayControlServer("", nil); err != nil {
		t.Fatal(err)
	}
	runner.SetSessionControlServer(control)
	if !control.Start() {
		t.Fatal("the control server did not start")
	}
	if err := gatewayrun.StartGatewayRuntimeAPI(ctx, runner); err != nil {
		t.Fatal(err)
	}
	if started, err := runner.Start(ctx); err != nil || !started {
		t.Fatalf("start = %v, %v", started, err)
	}
	if err := gatewayrun.PublishGatewayRuntimeReady(runner); err != nil {
		t.Fatal(err)
	}
	createWithSessionToken(t, runner.SessionAPIOrigin(), dashboard.SessionToken(), "token-create", 10*time.Second)
	unbound, err := gateway.NewAuthorityConnection(runner.SessionAuthority(), bareTransport{}, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unbound.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	if capabilities := unbound.Actor().Capabilities; len(capabilities) != 0 {
		t.Fatalf("capabilities = %v", capabilities)
	}
}
