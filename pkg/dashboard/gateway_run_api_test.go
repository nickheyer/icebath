package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Runner of the listener cases, the SimpleNamespace runner the hermes cases build
type fakeAPIRunner struct {
	descriptor map[string]any
	draining   atomic.Bool
	authority  *gateway.SessionAuthority
	tickets    *gateway.TicketStore
	activity   atomic.Int32
	attached   atomic.Int32
}

func (r *fakeAPIRunner) SessionRuntimeDescriptor() map[string]any    { return r.descriptor }
func (r *fakeAPIRunner) Draining() bool                              { return r.draining.Load() }
func (r *fakeAPIRunner) Running() bool                               { return true }
func (r *fakeAPIRunner) SessionAuthority() *gateway.SessionAuthority { return r.authority }
func (r *fakeAPIRunner) SessionTicketStore() *gateway.TicketStore    { return r.tickets }
func (r *fakeAPIRunner) NoteClientActivity()                         { r.activity.Add(1) }
func (r *fakeAPIRunner) ListenerLoop() *gateway.CallbackLoop         { return nil }
func (r *fakeAPIRunner) AttachClient() func() {
	r.attached.Add(1)
	return func() { r.attached.Add(-1) }
}

func TestTheLogLineNamesTheStatusTheDialerGot(t *testing.T) {
	for name, refusedFirst := range map[string]bool{"raised first": false, "refused first": true} {
		t.Run(name, func(t *testing.T) {
			records, _ := captureLogs(t, slog.LevelError)
			fail := func(ctx context.Context, _ *Scope, _ Receive, send Send) error {
				if refusedFirst {
					if err := send(ctx, Message{Type: "websocket.close", Code: 1008}); err != nil {
						return err
					}
				}
				return &RuntimeError{Msg: "late failure"}
			}
			api := &gatewayRuntimeAPI{app: fail, runner: &fakeAPIRunner{}}
			var sent []Message
			send := func(_ context.Context, m Message) error {
				sent = append(sent, m)
				return nil
			}
			scope := &Scope{Type: "websocket", Path: "/api/ws", Client: &Address{Host: "127.0.0.1", Port: 5}}
			err := api.ASGI(context.Background(), scope, nil, send)
			var raised *RuntimeError
			if !errors.As(err, &raised) || raised.Msg != "late failure" {
				t.Fatalf("err = %v", err)
			}
			var lines []string
			for _, rec := range records() {
				if rec.logger == "gateway.run_api" && strings.Contains(rec.message, "session WebSocket upgrade") {
					lines = append(lines, rec.message)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("lines = %q", lines)
			}
			if !strings.Contains(lines[0], "RuntimeError: late failure") {
				t.Errorf("line = %q", lines[0])
			}
			if strings.Contains(lines[0], "HTTP 500") == refusedFirst {
				t.Errorf("line = %q", lines[0])
			}
			if len(sent) != map[bool]int{false: 0, true: 1}[refusedFirst] {
				t.Errorf("sent = %v", sent)
			}
		})
	}
}

func TestCloseDuringLifespanEntryUnmountsAndExitsTheLifespan(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var exits atomic.Int32
	app := NewFastAPI("fixture", "0", func(context.Context) error {
		close(entered)
		<-release
		return nil
	}, func(context.Context) error {
		exits.Add(1)
		return nil
	})
	app.State.Set("gateway_runner", nil)
	prevApp, prevGate := mountWebServerApp, mountConfigureAuthGate
	t.Cleanup(func() { mountWebServerApp, mountConfigureAuthGate = prevApp, prevGate })
	mountWebServerApp = func() (*FastAPI, error) { return app, nil }
	mountConfigureAuthGate = func(context.Context, *FastAPI, string, bool, *string, *string) error { return nil }
	runner := &fakeAPIRunner{authority: &gateway.SessionAuthority{}}
	mount := &gatewayDashboardMount{runner: runner, state: &gateway.ListenerState{BoundHost: "127.0.0.1", BoundPort: 1}}
	ctx := context.Background()
	warm := make(chan error, 1)
	go func() {
		_, err := mount.Ensure(ctx)
		warm <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the lifespan never started")
	}
	if app.State.Get("gateway_runner") != any(runner) {
		t.Fatalf("gateway_runner = %v", app.State.Get("gateway_runner"))
	}
	if err := mount.Close(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	err := <-warm
	var stopped *gateway.PyError
	if !errors.As(err, &stopped) || stopped.Kind != "RuntimeError" || !strings.Contains(stopped.Msg, "stopped") {
		t.Fatalf("err = %v", err)
	}
	if mount.Web() != nil {
		t.Error("the mount stayed loaded")
	}
	if got := exits.Load(); got != 1 {
		t.Errorf("exits = %d", got)
	}
	if app.State.Get("gateway_runner") != nil || app.State.Get("session_authority") != nil {
		t.Errorf("runner = %v, authority = %v", app.State.Get("gateway_runner"), app.State.Get("session_authority"))
	}
}

// Marks the fake runner draining for a restart after turn wait, the descriptor changed in place as the gateway does
func (r *fakeAPIRunner) openRestartWait() {
	r.draining.Store(true)
	r.descriptor["state"] = "draining"
	r.descriptor["drain_reason"] = gateway.RestartWaitReason
}

func TestRestartWaitCutsSocketsOpenedBeforeIt(t *testing.T) {
	for name, openedInWait := range map[string]bool{"opened while ready": false, "opened in the wait": true} {
		t.Run(name, func(t *testing.T) {
			app := dashboardTestApp(t)
			h := t.TempDir()
			tickets, err := gateway.NewTicketStore("owner", map[string]bool{h: true})
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := tickets.Mint(h, "human", "interactive", "", false)
			if err != nil {
				t.Fatal(err)
			}
			db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			runner := &fakeAPIRunner{descriptor: map[string]any{"state": "ready"}, tickets: tickets,
				authority: &gateway.SessionAuthority{DB: db, ProfileID: h}}
			if openedInWait {
				runner.openRestartWait()
			}
			type event struct {
				kind  string
				value any
			}
			var received []event
			previous := sessionHandleWS
			t.Cleanup(func() { sessionHandleWS = previous })
			sessionHandleWS = func(ctx context.Context, ws tuiserver.ServedSocket, _ map[string]any, _ *string, _ bool) error {
				protocol := gateway.CanonicalGatewayProtocol
				if err := ws.Accept(ctx, &protocol); err != nil {
					return err
				}
				runner.openRestartWait()
				for {
					text, err := ws.ReceiveText(ctx)
					var gone *tuiserver.SocketDisconnect
					if errors.As(err, &gone) {
						received = append(received, event{"disconnect", gone.Code})
						return nil
					}
					if err != nil {
						return err
					}
					received = append(received, event{"text", text})
				}
			}
			fallback := func(context.Context, *Scope, Receive, Send) error {
				t.Error("a ticketed native socket must not enter the legacy fallback")
				return nil
			}
			frame := `{"method": "prompt.receipt"}`
			incoming := []Message{{Type: "websocket.connect"}, {Type: "websocket.receive", Text: &frame},
				{Type: "websocket.disconnect", Code: 1000}}
			receive := func(context.Context) (Message, error) {
				next := incoming[0]
				incoming = incoming[1:]
				return next, nil
			}
			send := func(context.Context, Message) error { return nil }
			offered := gateway.CanonicalGatewayProtocol + ", " + gateway.GatewayWSTicketPrefix + ticket
			scope := &Scope{Type: "websocket", Path: "/api/ws", Scheme: "ws", Client: &Address{Host: "127.0.0.1", Port: 1234},
				Server: &Address{Host: "127.0.0.1", Port: 9999}, Headers: []Header{{"sec-websocket-protocol", offered}},
				Subprotocols: []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + ticket}}
			mount := &gatewayDashboardMount{runner: runner, state: &gateway.ListenerState{}, web: &mountedDashboardWeb{app: app}}
			api := &gatewayRuntimeAPI{app: fallback, runner: runner, mount: mount}
			if err := api.ASGI(context.Background(), scope, receive, send); err != nil {
				t.Fatal(err)
			}
			want := []event{{"disconnect", int64(1013)}}
			if openedInWait {
				want = []event{{"text", frame}, {"disconnect", int64(1000)}}
			}
			if !slices.Equal(received, want) {
				t.Fatalf("received = %v, want %v", received, want)
			}
		})
	}
}

func TestRestartWaitAdmitsABrowserTicketOnlyWhereItsGateRedeemsIt(t *testing.T) {
	app := dashboardTestApp(t)
	token := strings.Repeat("a", 43)
	previousToken := SessionToken()
	setSessionToken(token)
	t.Cleanup(func() { setSessionToken(previousToken) })
	app.State.Set("bound_host", "localhost")
	app.State.Set("auth_required", false)
	runner := &fakeAPIRunner{descriptor: map[string]any{"state": "draining", "drain_reason": gateway.RestartWaitReason}}
	runner.draining.Store(true)
	reached := 0
	previous := sessionHandleWS
	t.Cleanup(func() { sessionHandleWS = previous })
	sessionHandleWS = func(context.Context, tuiserver.ServedSocket, map[string]any, *string, bool) error {
		reached++
		return nil
	}
	var sent []Message
	receive := func(context.Context) (Message, error) { return Message{Type: "websocket.connect"}, nil }
	send := func(_ context.Context, m Message) error {
		sent = append(sent, m)
		return nil
	}
	protocols := []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + "not-a-ticket"}
	scope := &Scope{Type: "websocket", Path: "/api/ws", Scheme: "ws", QueryString: "token=" + token,
		Client: &Address{Host: "127.0.0.1", Port: 1234}, Server: &Address{Host: "localhost", Port: 80},
		Headers: []Header{{"host", "localhost"}, {"origin", "http://localhost"},
			{"sec-websocket-protocol", strings.Join(protocols, ", ")}}, Subprotocols: protocols}
	mount := &gatewayDashboardMount{runner: runner, state: &gateway.ListenerState{}, web: &mountedDashboardWeb{app: app}}
	api := &gatewayRuntimeAPI{app: app.ASGI, runner: runner, mount: mount}
	if err := api.ASGI(context.Background(), scope, receive, send); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Type != "websocket.close" || sent[0].Code != 1013 {
		t.Fatalf("sent = %+v", sent)
	}
	if reached != 0 {
		t.Errorf("the session handler ran %d time(s)", reached)
	}
}
