package gateway

import (
	"context"
	"net"
)

// Runner side the gateway listener admits sockets against, the runner attributes gateway/run_api.py reads
type GatewayAPIRunner interface {
	DrainRunner
	SessionAuthority() *SessionAuthority
	SessionTicketStore() *TicketStore
	// Marks dashboard or API traffic so an idle exit gateway stays up
	NoteClientActivity()
	// Counts an attached native client until the returned release runs
	AttachClient() (release func())
}

// Mounted dashboard application as the webapp control verbs read it, the web_server module of the mount
type GatewayDashboardWeb interface {
	// Bundle directory the mounted Webapp serves, false when the app state names none
	WebDist() (string, bool)
	// Whether a launch link opens the Webapp with no dashboard sign-in in front of it
	PrivateLaunch() bool
	// Public URL the listener answers to, empty when it declares none
	ListenerPublicURL() string
	// Whether the dashboard sign-in gate guards the application
	AuthRequired() bool
	// One use launch ticket a browser the server opens redeems
	MintLaunchTicket() (string, error)
	// One use launch ticket printed for the user, spent without the peer check
	MintPrintedLaunchTicket() (string, error)
	// Seconds a launch ticket stays redeemable
	LaunchTicketTTLSeconds() int
	// Makes credential the browser credential, grants ended by revoker name
	RotateBrowserCredential(credential string) map[string]int
}

// Dashboard application the gateway listener imports and mounts on first use
type GatewayDashboardMount interface {
	// Imports, binds and starts the dashboard once, returning the mounted application
	Ensure(ctx context.Context) (GatewayDashboardWeb, error)
	// Mounted application, nil before the mount loaded
	Web() GatewayDashboardWeb
	// Whether close ran, the listener having stopped
	Closed() bool
	// Exits the dashboard lifespan and unbinds the listener state from the application
	Close(ctx context.Context) error
}

// Uvicorn server of the gateway listener, its app the runtime API around the dashboard mount
type GatewayListenerServer interface {
	// Listen backlog of the server config
	Backlog() int
	// Whether the dashboard module was imported before this listener was built
	DashboardLoaded() bool
	// Dashboard mount the runtime API serves through
	Mount() GatewayDashboardMount
	// Loads the ASGI graph and wraps it with the runtime API
	Load(ctx context.Context) error
	// Starts serving the bound listener with the lifespan the config names
	Startup(ctx context.Context, listener net.Listener) error
	Started() bool
	ShouldExit() bool
	// Asks the main loop to return
	RequestExit()
	MainLoop(ctx context.Context) error
	// Drains the connections of the listener
	Shutdown(ctx context.Context, listener net.Listener) error
}
