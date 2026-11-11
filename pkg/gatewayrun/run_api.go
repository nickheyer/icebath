package gatewayrun

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func apiLog() *slog.Logger { return slog.Default().With("logger", "gateway.run_api") }

// Dashboard listener builder of pkg/dashboard, the mount and uvicorn server around the runtime API, set by the command wiring
var BuildGatewayListener func(ctx context.Context, runner gateway.GatewayAPIRunner, state *gateway.ListenerState,
	webapp *gateway.WebappMount, host string, port int) (gateway.GatewayListenerServer, error)

// Public URL the listener answers to, the Webapp's own and nil for main's listener
func listenerPublicURL(webapp *gateway.WebappMount) *string {
	if webapp == nil {
		return nil
	}
	u := webapp.PublicURL
	return &u
}

// First address host resolves to for a stream socket, the getaddrinfo entry the listener binds
func listenerAddress(ctx context.Context, host string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(addrs) == 0 {
		return netip.Addr{}, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return addrs[0].Unmap(), nil
}

// Listener socket opener, the real function swapped only where a hermes case installs a fake
var listenerSocketFn = openListenerSocket

// Binds the gateway's local listener serving the native session socket and the lazily mounted dashboard
func startGatewayAPI(ctx context.Context, r *GatewayRunner, host string, port int, webapp *gateway.WebappMount) (*gatewayAPIHandle, error) {
	if gateway.ActiveAPIRunner() != nil {
		return nil, errors.New("gateway API already started")
	}
	publicURL := listenerPublicURL(webapp)
	var trusted map[string]bool
	var authRequired bool
	if publicURL == nil {
		trusted = gateway.DashboardPublicHosts(ctx)
		authRequired = gateway.ListenerAuthRequired(host, trusted)
	} else {
		trusted = gateway.PublicURLHosts(*publicURL)
		authRequired = gateway.ShouldRequireAuth(host) || gateway.PublicHostsEngageGate(trusted)
	}
	state := &gateway.ListenerState{BoundHost: host, AuthRequired: authRequired, TrustedPublicHosts: trusted}
	server, err := BuildGatewayListener(ctx, r, state, webapp, host, port)
	if err != nil {
		return nil, err
	}
	mount := server.Mount()
	addr, err := listenerAddress(ctx, host)
	if err != nil {
		return nil, err
	}
	sock, err := listenerSocketFn(addr, port, addr.Is6() && addr == netip.IPv6Unspecified())
	if err != nil {
		return nil, err
	}
	gateway.SetActiveAPIRunner(r)
	listener, err := func() (net.Listener, error) {
		if err := sock.bind(addr, port); err != nil {
			return nil, err
		}
		if port != 0 {
			if _, err := sock.listen(server.Backlog()); err != nil {
				return nil, err
			}
		}
		_, state.BoundPort = sock.sockname()
		if err := server.Load(ctx); err != nil {
			return nil, err
		}
		if state.AuthRequired || server.DashboardLoaded() || webapp != nil {
			if _, err := mount.Ensure(ctx); err != nil {
				return nil, err
			}
		}
		listener, err := sock.listen(server.Backlog())
		if err != nil {
			return nil, err
		}
		if err := server.Startup(ctx, listener); err != nil {
			return nil, err
		}
		if !server.Started() || server.ShouldExit() {
			return nil, errors.New("gateway API lifespan startup failed")
		}
		return listener, nil
	}()
	if err != nil {
		sock.close()
		gateway.SetActiveAPIRunner(nil)
		return nil, errors.Join(err, mount.Close(ctx))
	}
	boundIP, _ := sock.sockname()
	handle := &gatewayAPIHandle{apiOrigin: gateway.WebappDialOrigin(boundIP, state.BoundPort), server: server,
		task: make(chan struct{}), app: mount, socket: listener}
	serveCtx := context.WithoutCancel(ctx)
	go func() {
		defer close(handle.task)
		err := server.MainLoop(serveCtx)
		err = errors.Join(err, server.Shutdown(serveCtx, listener))
		listener.Close()
		err = errors.Join(err, mount.Close(serveCtx))
		gateway.SetActiveAPIRunner(nil)
		handle.serveErr = err
	}()
	return handle, nil
}

// Mounts the dashboard in the background once a supervised gateway is ready, logging a failed warm up
func warmGatewayDashboard(ctx context.Context, r *GatewayRunner) {
	handle := r.sessionAPI
	if handle == nil || handle.app == nil || handle.app.Web() != nil {
		return
	}
	mount := handle.app
	r.gatewayDashboardWarmup = spawnGatewayTask(ctx, "gateway-dashboard-warmup", func(ctx context.Context) (any, error) {
		if _, err := mount.Ensure(ctx); err != nil && !mount.Closed() {
			apiLog().Warn("Gateway dashboard API warm-up failed; it loads on first request", "exc_info", err)
		}
		return nil, nil
	})
}
