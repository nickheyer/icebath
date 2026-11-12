package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysignal"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

const (
	loopHeartbeatIntervalS = 2.0
	loopHeartbeatStallS    = 5.0
	idleWatchdogPollS      = 15.0
)

// Module functions the server start resolves at call time, replaced only where a hermes case patches them
var (
	startConfigureAuthGate  = configureAuthGate
	startBuildUvicornServer = buildUvicornServer
	startPortBindConflict   = portBindConflict
	startRunServe           = runServe
	startWriteSentinelLine  = writeMachineSentinelLine
)

// Drops the proxy variables an update replay added, the updater function assigned by the command wiring
var RestoreUserProxyEnv func(environ map[string]string)

// Pointer to a launch string, nil for the empty string that stands for None
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Idle grace seconds of an SSH isolated backend from dashboard.ssh_isolated_idle_grace_s, the default when not a number
func sshIsolatedIdleGrace(ctx context.Context) (float64, error) {
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return 0, err
	}
	section := map[string]any{}
	if raw := cfg["dashboard"]; pyrepr.Truthy(raw) {
		dict, isDict := raw.(map[string]any)
		if !isDict {
			return 0, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		section = dict
	}
	raw, present := section["ssh_isolated_idle_grace_s"]
	if !present {
		return DefaultIdleGraceS, nil
	}
	grace, err := pyint.Float(raw)
	var conversion *pyint.Error
	if errors.As(err, &conversion) && (conversion.Kind == "TypeError" || conversion.Kind == "ValueError") {
		return DefaultIdleGraceS, nil
	}
	return grace, err
}

// Arms the two second tick on the serving loop whose drift past five seconds is logged as a stall
func startLoopHeartbeat(loop *gateway.CallbackLoop) {
	var beat func(expected float64)
	beat = func(expected float64) {
		now := monotonic()
		if drift := now - expected; drift > loopHeartbeatStallS {
			webServerLog().Warn(fmt.Sprintf("event loop stalled %.1fs (GIL pressure suspected)", drift))
		}
		next := now + loopHeartbeatIntervalS
		_, _ = loop.CallLater(secondsDuration(loopHeartbeatIntervalS), func() { beat(next) })
	}
	first := monotonic() + loopHeartbeatIntervalS
	_, _ = loop.CallLater(secondsDuration(loopHeartbeatIntervalS), func() { beat(first) })
}

// Arms the delayed MCP discovery of a Desktop serve, a failure to arm only a debug line
func deferDesktopMCPDiscovery(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Debug("Deferred MCP discovery arm failed", "exc_info", r)
		}
	}()
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	delay := desktopMCPDiscoveryDelay
	mcp.DeferBackgroundDiscovery(ctx, &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}, webServerLog(),
		"dashboard-mcp-discovery", &delay)
}

// Arms everything that follows the bind, reaps, watchdogs, identity, rendezvous, the ready lines, browser, MCP and heartbeat
func onServerStarted(ctx context.Context, app *FastAPI, server *UvicornServer, o gateway.DashboardServerOptions) error {
	desktopOwned := tools.IsDesktopOwnedBackend(nil)
	if desktopOwned {
		bestEffort("orphan desktop-local serve reap", func() error {
			gateway.ReapOrphanedDesktopLocalServes(ctx, o.Stderr, gateway.DashboardReapOptions{})
			return nil
		})
	}
	bestEffort("orphan MCP helper reap", func() error {
		tools.ReapOrphanedMCPHelpers("", nil)
		return nil
	})
	startParentDeathWatchdog()
	tracker, _ := app.State.Get("ssh_isolated_clients").(*IdleClientTracker)
	if tracker != nil {
		grace, err := sshIsolatedIdleGrace(ctx)
		if err != nil {
			return err
		}
		StartIdleWatchdog(&server.ShouldExit, tracker, grace, idleWatchdogPollS, nil)
	}
	if tracker != nil || desktopOwned {
		StartCodeSkewWatchdog(&server.ShouldExit, nil, nil, nil, DefaultSkewPollS, unboundedRetirePolls)
	}
	if nonce := SSHOwnerNonce(); tracker != nil && o.SSHLockPath != "" && nonce != "" {
		StartOwnerWatchdog(&server.ShouldExit, o.SSHLockPath, nonce, nil, nil, DefaultOwnerPollS, nil, unboundedRetirePolls)
	}
	actualPort := readBoundPort(server, o.Port)
	app.State.Set("bound_port", actualPort)
	app.State.Set("serves_spa", !o.Headless)
	bestEffort("process-identity registration", func() error {
		purpose := "dashboard"
		if o.Headless {
			purpose = "serve"
		}
		tools.RegisterSelf(ctx, purpose, "", map[string]any{"host": o.Host, "port": actualPort,
			"profile": o.InitialProfile, "isolated": o.Isolated})
		tools.AttachSelfToKillOnCloseJob()
		return nil
	})
	bestEffort("host rendezvous publish", func() error {
		publishHostRendezvous(ctx, app, o.Host, actualPort)
		return nil
	})
	writeDashboardReadyFile(ctx, actualPort)
	if o.Headless {
		startWriteSentinelLine(fmt.Sprintf("ICEBATH_BACKEND_READY port=%d", actualPort), o.Stderr)
	}
	startWriteSentinelLine(fmt.Sprintf("ICEBATH_DASHBOARD_READY port=%d", actualPort), o.Stderr)
	if o.Headless {
		fmt.Fprintf(o.Stderr, "  Icebath backend listening on %s:%d\n", o.Host, actualPort)
	} else {
		fmt.Fprintf(o.Stderr, "  Icebath Web UI → http://%s:%d\n", pyurl.FormatURLHost(o.Host), actualPort)
	}
	maybeOpenBrowser(o.Host, actualPort, o.OpenBrowser, o.InitialProfile)
	if o.StartMCPDiscoveryAfterBind {
		deferDesktopMCPDiscovery(ctx)
	}
	bestEffort("loop noise filter install", func() error {
		tuiserver.InstallLoopNoiseFilter(server.Loop)
		return nil
	})
	startLoopHeartbeat(server.Loop)
	return nil
}

// Runs the serve step, a console interrupt ending clean, a lost bind race exiting with the port in use status
func runServe(serve func() error, o gateway.DashboardServerOptions) error {
	err := serve()
	if err == nil || errors.Is(err, pysignal.ErrKeyboardInterrupt) {
		return nil
	}
	var exit *gateway.DashboardExit
	if signalled := (*pysignal.SystemExit)(nil); errors.As(err, &signalled) {
		exit = &gateway.DashboardExit{Code: signalled.Code}
	} else if !errors.As(err, &exit) {
		return err
	}
	if exit.Code == 1 && exit.Msg == "" && startPortBindConflict(o.Host, o.Port) {
		reportPortInUse(o.Host, o.Port, o.Stderr)
		return &gateway.DashboardExit{Code: PortInUseExitCode}
	}
	return exit
}

// Starts the Nous auth keepalive for the launch home, a failure to start only a debug line
func startNousAuthKeepalive(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Debug(fmt.Sprintf("Nous auth keepalive did not start: %v", r))
		}
	}()
	provider.StartNousAuthKeepalive(provider.HomePoolEnv(home.Dir(ctx)), provider.NousKeepaliveStartOptions{})
}

// Installs the handlers that persist in memory transcripts on SIGTERM and SIGINT, a failure only a debug line
func installExitFlushHandlers() {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Debug(fmt.Sprintf("exit-flush signal handlers not installed: %v", r))
		}
	}()
	tuiserver.InstallExitFlushSignalHandlers()
}

// Decides multi profile hosting as the last boot step, a failure logged as a warning
func activateMultiProfileHosting(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			webServerLog().Warn("eager multi-profile activation failed", "exc_info", r)
		}
	}()
	if _, err := provider.ActivateMultiProfileHostingEagerly(ctx); err != nil {
		webServerLog().Warn("eager multi-profile activation failed", "exc_info", err)
	}
}

// Serves the application on host and port until an exit is requested, running every boot and post bind step
func startServer(ctx context.Context, app *FastAPI, o gateway.DashboardServerOptions) error {
	RestoreUserProxyEnv(nil)
	applySSHSessionToken(o.SSHSessionToken)
	applySSHOwnerNonce(o.SSHOwnerNonce)
	gateway.ApplyNofileSoftLimit(nil)
	startNousAuthKeepalive(ctx)
	if err := startConfigureAuthGate(ctx, app, o.Host, o.AllowPublic, optionalString(o.SSHSessionToken), optionalString(o.SSHOwnerNonce)); err != nil {
		return err
	}
	app.State.Set("bound_host", o.Host)
	app.State.Set("initial_profile", o.InitialProfile)
	_, server := startBuildUvicornServer(ctx, app, o.Host, o.Port, o.SSHSessionToken != "")
	installExitFlushHandlers()
	if startPortBindConflict(o.Host, o.Port) {
		reportPortInUse(o.Host, o.Port, o.Stderr)
		return &gateway.DashboardExit{Code: PortInUseExitCode}
	}
	activateMultiProfileHosting(ctx)
	serve := func() error {
		loopCtx, stopLoop := context.WithCancel(context.WithoutCancel(ctx))
		go server.Loop.Run(loopCtx)
		defer func() {
			stopLoop()
			server.Loop.Close()
		}()
		return server.CaptureSignals(func() error {
			if err := server.Startup(ctx); err != nil {
				return err
			}
			if server.ShouldExit.Load() {
				return nil
			}
			if err := onServerStarted(ctx, app, server, o); err != nil {
				return err
			}
			if o.Headless {
				observability.RecordProcessReady(ctx, "serve_boot", true)
			}
			server.MainLoop(ctx)
			if server.Started {
				server.Shutdown(ctx)
			}
			return nil
		})
	}
	return startRunServe(serve, o)
}
