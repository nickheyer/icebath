package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func gatewayRunAPILog() *slog.Logger { return slog.Default().With("logger", "gateway.run_api") }

// Module application and auth gate a mount binds through, replaced only where a hermes case fakes the module
var (
	mountWebServerApp      = WebServerApp
	mountConfigureAuthGate = configureAuthGate
)

func gatewayAPIStopped() error {
	return &gateway.PyError{Kind: "RuntimeError", Msg: "gateway API stopped"}
}

// Mounted dashboard application as the gateway reads it, the web server module of a loaded mount
type mountedDashboardWeb struct{ app *FastAPI }

func (w *mountedDashboardWeb) WebDist() (string, bool) {
	dist, ok := w.app.State.Get("web_dist").(string)
	return dist, ok
}

func (w *mountedDashboardWeb) PrivateLaunch() bool { return PrivateLaunch(w.app.State) }

func (w *mountedDashboardWeb) ListenerPublicURL() string {
	return w.app.State.String("listener_public_url")
}

func (w *mountedDashboardWeb) AuthRequired() bool { return w.app.State.Bool("auth_required") }

func (w *mountedDashboardWeb) MintLaunchTicket() (string, error) {
	return mintLaunchTicket(w.app.State)
}

func (w *mountedDashboardWeb) MintPrintedLaunchTicket() (string, error) {
	return mintPrintedLaunchTicket(w.app.State)
}

func (w *mountedDashboardWeb) LaunchTicketTTLSeconds() int { return webappLaunchTicketTTLSeconds }

// Makes credential the browser credential and returns how many grants each revoker ended
func (w *mountedDashboardWeb) RotateBrowserCredential(credential string) map[string]int {
	ended := map[string]int{}
	counts := BrowserCredentialRotate(credential)
	for _, name := range counts.Keys {
		ended[name], _ = counts.GetOr(name, 0).(int)
	}
	return ended
}

// Dashboard application the gateway listener imports and mounts on first use, the listener boundary state mirrored onto it
type gatewayDashboardMount struct {
	runner   gateway.GatewayAPIRunner
	state    *gateway.ListenerState
	webapp   *gateway.WebappMount
	ensureMu sync.Mutex
	mu       sync.Mutex
	web      *mountedDashboardWeb
	// Exit half of the dashboard lifespan this mount entered, nil until it did
	lifespanExit         func(ctx context.Context) error
	closed               bool
	replacedSessionToken *string
	undoOwnSignIn        func()
}

// Unloaded mount over the listener boundary state of runner, the Webapp it serves when webapp is set
func newGatewayDashboardMount(runner gateway.GatewayAPIRunner, state *gateway.ListenerState, webapp *gateway.WebappMount) *gatewayDashboardMount {
	return &gatewayDashboardMount{runner: runner, state: state, webapp: webapp}
}

func (m *gatewayDashboardMount) loaded() *mountedDashboardWeb {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.web
}

func (m *gatewayDashboardMount) Web() gateway.GatewayDashboardWeb {
	if web := m.loaded(); web != nil {
		return web
	}
	return nil
}

func (m *gatewayDashboardMount) Closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Boundary state the socket checks read, the application state once mounted and the listener snapshot before
func (m *gatewayDashboardMount) boundaryState() *gateway.ListenerState {
	if web := m.loaded(); web != nil {
		return listenerStateOf(web.app.State)
	}
	return m.state
}

// Whether the serving surface redeems browser minted gateway tickets, the gateway surface before the mount
func (m *gatewayDashboardMount) gatewayTicketSurface() bool {
	if web := m.loaded(); web != nil {
		return PolicyOf(web.app.State).GatewayTicket
	}
	return SurfacePolicies["gateway"].GatewayTicket
}

// Mirrors the listener state onto the application, the Webapp credential first so a home that cannot store it sets nothing
func (m *gatewayDashboardMount) bindState(ctx context.Context, app *FastAPI) error {
	token := ""
	if m.webapp != nil {
		var err error
		if token, err = gateway.BrowserSessionToken(ctx, m.webapp.Home, false); err != nil {
			return err
		}
	}
	app.State.Set("withhold_session_token", true)
	if m.webapp != nil {
		app.State.Set("listener_public_url", m.webapp.PublicURL)
	} else {
		app.State.Del("listener_public_url")
	}
	if m.webapp != nil {
		undo := gateway.UseOwnSignIn(m.webapp)
		m.mu.Lock()
		m.undoOwnSignIn = undo
		m.mu.Unlock()
	}
	if err := mountConfigureAuthGate(ctx, app, m.state.BoundHost, false, nil, nil); err != nil {
		return err
	}
	app.State.Set("gateway_runner", m.runner)
	if authority := m.runner.SessionAuthority(); authority != nil {
		app.State.Set("session_authority", authority)
	} else {
		app.State.Set("session_authority", nil)
	}
	app.State.Set("bound_host", m.state.BoundHost)
	app.State.Set("bound_port", m.state.BoundPort)
	if m.webapp != nil {
		app.State.Set("ui_surface", "webapp")
		app.State.Set("web_dist", m.webapp.Dist)
		app.State.Set("webapp_home", m.webapp.Home)
		replaced := SessionToken()
		m.mu.Lock()
		m.replacedSessionToken = &replaced
		m.mu.Unlock()
		setSessionToken(token)
	}
	return nil
}

// Undoes the bind on app, own sign in client, runner and listener keys, the Webapp surface and token
func (m *gatewayDashboardMount) unbindState(app *FastAPI) {
	m.mu.Lock()
	undo, replaced := m.undoOwnSignIn, m.replacedSessionToken
	m.undoOwnSignIn = nil
	if app != nil {
		m.replacedSessionToken = nil
	}
	m.mu.Unlock()
	if undo != nil {
		undo()
	}
	if app == nil {
		return
	}
	app.State.Set("gateway_runner", nil)
	app.State.Set("session_authority", nil)
	app.State.Set("withhold_session_token", false)
	app.State.Del("listener_public_url")
	if replaced != nil {
		for _, name := range []string{"ui_surface", "web_dist", "webapp_home"} {
			app.State.Del(name)
		}
		setSessionToken(*replaced)
	}
}

// Binds the listener state and enters the dashboard lifespan, a close that won meanwhile leaving it again
func (m *gatewayDashboardMount) bindAndEnter(ctx context.Context, app *FastAPI) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if cause, isErr := r.(error); isErr {
				err = cause
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	if err := m.bindState(ctx, app); err != nil {
		return err
	}
	if enter := app.Router.lifespanEnter; enter != nil {
		if err := enter(ctx); err != nil {
			return err
		}
	}
	if m.Closed() {
		if exit := app.Router.lifespanExit; exit != nil {
			if err := exit(ctx); err != nil {
				return err
			}
		}
		return gatewayAPIStopped()
	}
	return nil
}

// Imports, binds and starts the dashboard application once and returns it mounted
func (m *gatewayDashboardMount) Ensure(ctx context.Context) (gateway.GatewayDashboardWeb, error) {
	if web := m.loaded(); web != nil {
		return web, nil
	}
	m.ensureMu.Lock()
	defer m.ensureMu.Unlock()
	if web := m.loaded(); web != nil {
		return web, nil
	}
	if m.Closed() {
		return nil, gatewayAPIStopped()
	}
	app, err := mountWebServerApp()
	if err != nil {
		return nil, err
	}
	if m.Closed() {
		return nil, gatewayAPIStopped()
	}
	if bound := app.State.Get("gateway_runner"); bound != nil && bound != any(m.runner) {
		return nil, &gateway.PyError{Kind: "RuntimeError", Msg: "gateway API already started"}
	}
	if err := m.bindAndEnter(ctx, app); err != nil {
		m.unbindState(app)
		return nil, err
	}
	web := &mountedDashboardWeb{app: app}
	m.mu.Lock()
	m.web, m.lifespanExit = web, app.Router.lifespanExit
	m.mu.Unlock()
	return web, nil
}

// Marks the mount closed, leaves the dashboard lifespan it entered and unbinds the listener state
func (m *gatewayDashboardMount) Close(ctx context.Context) (err error) {
	m.mu.Lock()
	m.closed = true
	exit, web := m.lifespanExit, m.web
	m.lifespanExit = nil
	m.mu.Unlock()
	defer func() {
		if web != nil {
			m.unbindState(web.app)
		} else {
			m.unbindState(nil)
		}
	}()
	if exit != nil {
		return exit(ctx)
	}
	return nil
}

// Serves one connection through the mounted application, answering 1011 or 500 when the dashboard cannot load
func (m *gatewayDashboardMount) ASGI(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type != "http" && scope.Type != "websocket" {
		return nil
	}
	web := m.loaded()
	if web == nil {
		if _, err := m.Ensure(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			gatewayRunAPILog().Error("Gateway dashboard API failed to load", "exc_info", err)
			if scope.Type == "websocket" {
				return send(ctx, Message{Type: "websocket.close", Code: 1011})
			}
			return MustJSONResponse(pyjson.ObjectOf("error", "dashboard_unavailable"), 500).Serve(ctx, scope, receive, send)
		}
		web = m.loaded()
	}
	return web.app.ASGI(ctx, scope, receive, send)
}
