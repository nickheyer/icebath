package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
)

// Handlers in the slots of the signals a listener start must not take over
func listenerSignalHandlers() map[os.Signal]any {
	out := map[os.Signal]any{}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		out[sig] = dashboard.SignalHandler(sig)
	}
	return out
}

func TestOwnerServesExistingRoutesAndWSWithoutSignalOwnership(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) { ownerServesExistingRoutesAndWS(t, shared) })
	}
}

func ownerServesExistingRoutesAndWS(t *testing.T, shared bool) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "home"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", user)
	if err := dashboard.ImportWebServer(); err != nil {
		t.Fatal(err)
	}
	web, err := dashboard.WebServerApp()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	config := gateway.DefaultGatewayConfig(ctx)
	config.SessionsDir = filepath.Join(tmp, "gateway-sessions")
	runner, err := gatewayrun.NewGatewayRunner(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if n := runner.Adapters().Len(); n != 0 {
		t.Fatalf("adapters = %d", n)
	}
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": map[string]any{}}
	var authority *gateway.SessionAuthority
	var ref gateway.SessionRef
	if shared {
		fixtureUser := "fixture"
		if authority, err = gateway.InitializeSessionAuthority(ctx, runner, "fixture", "api-owner", nil, true); err != nil {
			t.Fatal(err)
		}
		if ref, err = authority.Register(ctx, &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "api-test", ChatType: "dm", UserID: &fixtureUser}); err != nil {
			t.Fatal(err)
		}
		request["method"], request["params"] = "session.resume", map[string]any{"session_id": ref.SessionID}
	}
	before := listenerSignalHandlers()
	handle, err := gatewayrun.StartGatewayAPI(ctx, runner, "127.0.0.1", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		gatewayrun.StopGatewayAPI(ctx, handle)
		if db := runner.SessionDB(ctx); db != nil {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}
		stopped = true
	}
	defer func() {
		if !stopped {
			stop()
		}
	}()
	port, _ := web.State.Get("bound_port").(int)
	if handle.APIOrigin() != fmt.Sprintf("http://127.0.0.1:%d", port) || port <= 0 {
		t.Fatalf("api_origin %s, bound_port %v", handle.APIOrigin(), web.State.Get("bound_port"))
	}
	if web.State.Get("gateway_runner") != gateway.GatewayAPIRunner(runner) {
		t.Fatalf("gateway_runner = %v", web.State.Get("gateway_runner"))
	}
	if after := listenerSignalHandlers(); !reflect.DeepEqual(before, after) {
		t.Fatalf("signal slots changed by the start: %v then %v", before, after)
	}
	auth := http.Header{"Authorization": {"Bearer " + dashboard.SessionToken()}}
	client := newOriginClient(t, handle.APIOrigin())
	status, _ := client.call("GET", "/api/config/raw", nil, "")
	wantStatus(t, "config without a token", status, 401)
	status, body := client.call("GET", "/api/config/raw", auth, "")
	wantStatus(t, "config with the token "+body, status, 200)
	if status, _ = client.call("POST", "/api/ws-ticket", nil, ""); status != 401 && status != 403 {
		t.Fatalf("ws-ticket without a token = %d", status)
	}
	status, _ = client.call("GET", "/api/config/raw", http.Header{"Authorization": auth["Authorization"], "Host": {"attacker.invalid"}}, "")
	wantStatus(t, "foreign Host", status, 400)
	url := strings.Replace(handle.APIOrigin(), "http:", "ws:", 1) + "/api/ws"
	if ws, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		ws.Close()
		t.Fatal("a socket without a token was accepted")
	} else if !errors.Is(err, websocket.ErrBadHandshake) || response == nil || response.StatusCode != 403 {
		t.Fatalf("socket without a token = %v %v", err, response)
	}
	ws, _, err := websocket.DefaultDialer.Dial(url+"?token="+dashboard.SessionToken(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	for reply["id"] != float64(1) {
		reply = nil
		if err := ws.ReadJSON(&reply); err != nil {
			t.Fatal(err)
		}
	}
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply = %v", reply)
	}
	if shared {
		if epoch, _ := result["authority_epoch"].(float64); int64(epoch) != authority.Epoch || result["session_id"] != ref.SessionID {
			t.Fatalf("resume = %v, epoch %d", result, authority.Epoch)
		}
		if subscribers := len(authority.Sessions[ref.SessionID].Subscribers); subscribers != 1 {
			t.Fatalf("subscribers = %d", subscribers)
		}
	}
	ws.Close()
	if _, err := gatewayrun.StartGatewayAPI(ctx, runner, "127.0.0.1", 0, nil); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second start = %v", err)
	}
	stop()
	gatewayrun.StopGatewayAPI(ctx, handle)
	if web.State.Get("session_authority") != nil {
		t.Fatalf("session_authority = %v", web.State.Get("session_authority"))
	}
	if after := listenerSignalHandlers(); !reflect.DeepEqual(before, after) {
		t.Fatalf("signal slots changed after the stop: %v then %v", before, after)
	}
	if _, err := http.Get(handle.APIOrigin() + "/api/config/raw"); err == nil {
		t.Fatal("the stopped listener still answers")
	}
}
