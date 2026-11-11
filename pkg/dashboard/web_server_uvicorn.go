package dashboard

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Uvicorn config and server of the dashboard bind, tunnel pings and client tracking for an SSH isolated backend
func buildUvicornServer(ctx context.Context, app *FastAPI, host string, port int, sshIsolated bool) (*UvicornConfig, *UvicornServer) {
	isLoopback := gateway.LoopbackHostValues[host]
	dashCfg := dashboardConfigSection(ctx)
	servedApp := app.ASGI
	var pingInterval, pingTimeout *time.Duration
	if !isLoopback {
		pingInterval, pingTimeout = wsPingSettingOf(dashCfg, "ws_ping_interval"), wsPingSettingOf(dashCfg, "ws_ping_timeout")
	}
	if sshIsolated {
		tracker := NewIdleClientTracker(nil)
		app.State.Set("ssh_isolated_clients", tracker)
		servedApp = WrapASGIWithWSTracking(app.ASGI, tracker)
		interval, timeout := secondsDuration(TunnelWSPingIntervalS), secondsDuration(TunnelWSPingTimeoutS)
		pingInterval, pingTimeout = &interval, &timeout
	}
	installUvicornConsoleHandlers()
	configureUvicornLogLevel(logging.LevelWarning)
	authRequired := app.State.Bool("auth_required")
	cfg := &UvicornConfig{App: servedApp, Host: host, Port: port, ProxyHeaders: authRequired,
		ForwardedAllowIPs: dashboardForwardedAllowIPs(dashCfg), WSPingInterval: pingInterval, WSPingTimeout: pingTimeout,
		WSPerMessageDeflate: sshIsolated || !isLoopback || authRequired, WSMaxSize: gateway.DesktopAttachmentWSMaxBytes,
		TimeoutGracefulShutdown: defaultListenerGracefulShutdown, Lifespan: LifespanAuto}
	return cfg, NewUvicornServer(cfg)
}
