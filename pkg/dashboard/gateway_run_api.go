package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

const gatewaySessionSocketPath = "/api/ws"

// Session socket handler an admitted native connection runs, replaced only where a hermes case patches handle_ws
var sessionHandleWS = tuiserver.HandleWS

// Runtime API of the gateway listener, private local tickets redeemed at the session socket ahead of the dashboard gate
type gatewayRuntimeAPI struct {
	app    ASGIApp
	runner gateway.GatewayAPIRunner
	mount  *gatewayDashboardMount
}

// Runtime API serving app for runner, the socket checks reading the boundary state of mount
func newGatewayRuntimeAPI(app ASGIApp, runner gateway.GatewayAPIRunner, mount *gatewayDashboardMount) *gatewayRuntimeAPI {
	return &gatewayRuntimeAPI{app: app, runner: runner, mount: mount}
}

// Python text of a scope peer, the host and port tuple or None
func socketPeerText(peer *Address) string {
	if peer == nil {
		return "None"
	}
	return fmt.Sprintf("(%s, %d)", pyrepr.Repr(peer.Host), peer.Port)
}

// Serves one connection, a session socket upgrade that fails before its accept logged with its peer and cause
func (g *gatewayRuntimeAPI) ASGI(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type != "websocket" || scope.Path != gatewaySessionSocketPath {
		return g.serve(ctx, scope, receive, send)
	}
	var started, accepted atomic.Bool
	sendWatched := func(ctx context.Context, m Message) error {
		started.Store(true)
		if m.Type == "websocket.accept" {
			accepted.Store(true)
		}
		return send(ctx, m)
	}
	err := g.serve(ctx, scope, receive, sendWatched)
	if err != nil && !errors.Is(err, context.Canceled) && !accepted.Load() {
		peer, _ := scope.Get(GatewaySocketPeerKey).(*Address)
		if peer == nil {
			peer = scope.Client
		}
		how := "with HTTP 500"
		if started.Load() {
			how = "after its handler had refused it"
		}
		gatewayRunAPILog().Error(fmt.Sprintf("Gateway refused a session WebSocket upgrade from %s %s: %s: %s",
			socketPeerText(peer), how, pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)), "exc_info", err)
	}
	return err
}

// Gates a connection on the runtime state, admits a ticketed local session socket, hands the rest to the dashboard
func (g *gatewayRuntimeAPI) serve(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	descriptor := g.runner.SessionRuntimeDescriptor()
	if (scope.Type != "http" && scope.Type != "websocket") || descriptor == nil {
		return g.app(ctx, scope, receive, send)
	}
	var stateMu sync.Mutex
	lastState := descriptor["state"]
	runtimeState := func() any {
		stateMu.Lock()
		defer stateMu.Unlock()
		if live := g.runner.SessionRuntimeDescriptor(); live != nil {
			lastState = live["state"]
		}
		return lastState
	}
	notReady := func() bool { return runtimeState() != "ready" || g.runner.Draining() }
	sessionSocket := scope.Type == "websocket" && scope.Path == gatewaySessionSocketPath
	state := descriptor["state"]
	drainingSocket := (state == "ready" || state == "draining") && (g.runner.Draining() || state == "draining") && sessionSocket
	if (state != "ready" || g.runner.Draining()) && !drainingSocket &&
		!(scope.Type == "http" && gateway.RestartWaitRead(g.runner, scope.Method, scope.Path)) &&
		!(scope.Type == "http" && gateway.RestartWaitSocketTicket(g.runner, scope.Method, scope.Path, scope.QueryString)) {
		if scope.Type == "websocket" {
			return send(ctx, Message{Type: "websocket.close", Code: 1013})
		}
		return MustJSONResponse(pyjson.ObjectOf("error", "gateway_not_ready", "state", state), 503).Serve(ctx, scope, receive, send)
	}
	scope.Set(GatewaySocketPeerKey, scope.Client)
	if !sessionSocket {
		g.runner.NoteClientActivity()
		return g.app(ctx, scope, receive, send)
	}
	var allowDraining, admittedInWait atomic.Bool
	receiveAdmitted := func(ctx context.Context) (Message, error) {
		message, err := receive(ctx)
		if err != nil {
			return message, err
		}
		if notReady() && !allowDraining.Load() && !(admittedInWait.Load() && gateway.RestartWaitOpen(g.runner)) {
			return Message{Type: "websocket.disconnect", Code: 1013}, nil
		}
		return message, nil
	}
	boundary := g.mount.boundaryState()
	ws := NewWebSocket(scope, receiveAdmitted, send)
	ticket, reason := gateway.GatewayWSTicketFromSubprotocol(ws)
	origin := ws.Headers().Get("origin")
	if reason == "none" || origin != "" {
		if drainingSocket {
			_, internal := ws.QueryParams().Lookup("internal")
			admittedInWait.Store(reason == "ok" && !internal && g.mount.gatewayTicketSurface() && gateway.RestartWaitOpen(g.runner))
			if !admittedInWait.Load() {
				return ws.Close(ctx, 1013, "")
			}
		}
		return g.app(ctx, scope, receiveAdmitted, send)
	}
	client := ws.Client()
	if reason != "ok" || !dashboardEmbeddedChatEnabled || gateway.WSHostOriginReason(ws, boundary) != "" ||
		gateway.WSClientReason(ws, boundary) != "" || client == nil || (client.Host != "127.0.0.1" && client.Host != "::1") {
		return ws.Close(ctx, 4403, "")
	}
	operator := true
	store := g.runner.SessionTicketStore()
	grant, err := store.Redeem(ticket, nil, "interactive")
	if isPermissionError(err) {
		operator = false
		grant, err = store.Redeem(ticket, nil, "worker-adoption")
		if isPermissionError(err) {
			if drainingSocket {
				return ws.Close(ctx, 1013, "")
			}
			return g.app(ctx, scope, receiveAdmitted, send)
		}
	}
	if err != nil {
		return err
	}
	allowDraining.Store(!operator)
	admittedInWait.Store(gateway.RestartWaitOpen(g.runner) && !pyrepr.Truthy(grant["refuse_restart_wait"]))
	if notReady() && !allowDraining.Load() && !admittedInWait.Load() {
		return ws.Close(ctx, 1013, "")
	}
	profileID, _ := grant["profile_id"].(string)
	authority, err := gateway.AuthorityForProfileID(g.runner, profileID)
	if err != nil {
		return err
	}
	if authority == nil {
		return ws.Close(ctx, 4403, "")
	}
	scope.Set("hermes.session_authority", authority)
	profileScope, scoped := grant["scope"]
	if !scoped {
		profileScope = "profile"
	}
	owned := gateway.OwnerScope(ctx, authority, false)
	release := g.runner.AttachClient()
	defer release()
	protocol := gateway.CanonicalGatewayProtocol
	return sessionHandleWS(owned, &servedSocket{ctx: owned, ws: ws}, map[string]any{"user_id": grant["subject"],
		"provider": "local", "profile_id": grant["profile_id"], "instance_id": grant["instance_id"],
		"capabilities": grant["capabilities"], "native_bootstrap": true, "profile_scope": profileScope}, &protocol, operator)
}

// Uvicorn server of the gateway listener with its lifespan off, the runtime API around the dashboard mount as its app
type gatewayListenerServer struct {
	cfg             *UvicornConfig
	server          *UvicornServer
	mount           *gatewayDashboardMount
	runner          gateway.GatewayAPIRunner
	dashboardLoaded bool
}

func (l *gatewayListenerServer) Backlog() int { return l.cfg.Backlog }

func (l *gatewayListenerServer) DashboardLoaded() bool { return l.dashboardLoaded }

func (l *gatewayListenerServer) Mount() gateway.GatewayDashboardMount { return l.mount }

// Wraps the loaded server app with the runtime API outside the proxy headers middleware, where the socket peer is visible
func (l *gatewayListenerServer) Load(context.Context) error {
	l.server.app = newGatewayRuntimeAPI(l.server.app, l.runner, l.mount).ASGI
	return nil
}

func (l *gatewayListenerServer) Startup(ctx context.Context, listener net.Listener) error {
	return l.server.StartupSockets(ctx, listener)
}

func (l *gatewayListenerServer) Started() bool { return l.server.Started }

func (l *gatewayListenerServer) ShouldExit() bool { return l.server.ShouldExit.Load() }

func (l *gatewayListenerServer) RequestExit() { l.server.ShouldExit.Store(true) }

func (l *gatewayListenerServer) MainLoop(ctx context.Context) error {
	l.server.MainLoop(ctx)
	return nil
}

func (l *gatewayListenerServer) Shutdown(ctx context.Context, _ net.Listener) error {
	l.server.Shutdown(ctx)
	return nil
}

// Builds the gateway listener, the lazily mounted dashboard behind the runtime API, refused while another runner holds the application
func BuildGatewayListener(ctx context.Context, runner gateway.GatewayAPIRunner, state *gateway.ListenerState,
	webapp *gateway.WebappMount, host string, port int) (gateway.GatewayListenerServer, error) {
	loaded := webServerAppLoaded()
	if loaded {
		if app, err := WebServerApp(); err == nil && app.State.Get("gateway_runner") != nil {
			return nil, &gateway.PyError{Kind: "RuntimeError", Msg: "gateway API already started"}
		}
	}
	mount := newGatewayDashboardMount(runner, state, webapp)
	graceful := 5 * time.Second
	cfg, server := BuildListenerServer(ctx, mount.ASGI, host, port, ListenerOptions{AuthRequired: state.AuthRequired,
		TimeoutGracefulShutdown: &graceful, Lifespan: LifespanOff})
	if loop := runner.ListenerLoop(); loop != nil {
		server.Loop = loop
	}
	return &gatewayListenerServer{cfg: cfg, server: server, mount: mount, runner: runner, dashboardLoaded: loaded}, nil
}
