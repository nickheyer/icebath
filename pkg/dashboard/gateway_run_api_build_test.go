package dashboard

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Status and body a built listener answers one HTTP request with
func serveBuiltListener(t *testing.T, server gateway.GatewayListenerServer, path string) (int, string) {
	t.Helper()
	built, ok := server.(*gatewayListenerServer)
	if !ok {
		t.Fatalf("server = %T", server)
	}
	var status int
	var body strings.Builder
	receive := func(context.Context) (Message, error) { return Message{Type: "http.request"}, nil }
	send := func(_ context.Context, m Message) error {
		switch m.Type {
		case "http.response.start":
			status = m.Status
		case "http.response.body":
			body.Write(m.Body)
		}
		return nil
	}
	scope := &Scope{Type: "http", HTTPVersion: "1.1", Method: "GET", Scheme: "http", Path: path,
		Headers: []Header{{"host", "127.0.0.1"}}, Client: &Address{Host: "127.0.0.1", Port: 4}}
	if err := built.server.app(context.Background(), scope, receive, send); err != nil {
		t.Fatal(err)
	}
	return status, body.String()
}

func TestBuiltListenerRefusesUntilReadyMountsOnceAndUnbindsOnClose(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var enters, exits atomic.Int32
	app := NewFastAPI("fixture", "0", func(context.Context) error {
		enters.Add(1)
		return nil
	}, func(context.Context) error {
		exits.Add(1)
		return nil
	})
	prevApp, prevGate := mountWebServerApp, mountConfigureAuthGate
	t.Cleanup(func() { mountWebServerApp, mountConfigureAuthGate = prevApp, prevGate })
	mountWebServerApp = func() (*FastAPI, error) { return app, nil }
	mountConfigureAuthGate = func(context.Context, *FastAPI, string, bool, *string, *string) error { return nil }
	runner := &fakeAPIRunner{descriptor: map[string]any{"state": "starting"}, authority: &gateway.SessionAuthority{}}
	ctx := context.Background()
	server, err := BuildGatewayListener(ctx, runner, &gateway.ListenerState{BoundHost: "127.0.0.1", BoundPort: 9}, nil, "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Load(ctx); err != nil {
		t.Fatal(err)
	}
	mount := server.Mount()
	if mount.Web() != nil || mount.Closed() {
		t.Fatalf("fresh mount loaded = %v, closed = %v", mount.Web() != nil, mount.Closed())
	}
	status, body := serveBuiltListener(t, server, "/api/status")
	if status != 503 || !strings.Contains(body, `"gateway_not_ready"`) || !strings.Contains(body, `"starting"`) {
		t.Fatalf("not ready answer = %d %s", status, body)
	}
	if mount.Web() != nil || runner.activity.Load() != 0 {
		t.Fatalf("a refused request mounted = %v, activity = %d", mount.Web() != nil, runner.activity.Load())
	}
	runner.descriptor["state"] = "ready"
	for range 2 {
		if status, body = serveBuiltListener(t, server, "/missing"); status != 404 {
			t.Fatalf("mounted answer = %d %s", status, body)
		}
	}
	if mount.Web() == nil || enters.Load() != 1 || runner.activity.Load() != 2 {
		t.Fatalf("loaded = %v, enters = %d, activity = %d", mount.Web() != nil, enters.Load(), runner.activity.Load())
	}
	if app.State.Get("gateway_runner") != any(runner) || !app.State.Bool("withhold_session_token") || app.State.Get("bound_port") != 9 {
		t.Fatalf("bound state = %v %v %v", app.State.Get("gateway_runner"), app.State.Get("withhold_session_token"), app.State.Get("bound_port"))
	}
	if err := mount.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if !mount.Closed() || exits.Load() != 1 || app.State.Get("gateway_runner") != nil || app.State.Bool("withhold_session_token") {
		t.Fatalf("closed = %v, exits = %d, runner = %v", mount.Closed(), exits.Load(), app.State.Get("gateway_runner"))
	}
	again := newGatewayDashboardMount(runner, &gateway.ListenerState{}, nil)
	if err := again.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var stopped *gateway.PyError
	if _, err := again.Ensure(ctx); !errors.As(err, &stopped) || stopped.Msg != "gateway API stopped" {
		t.Fatalf("ensure after close = %v", err)
	}
}
