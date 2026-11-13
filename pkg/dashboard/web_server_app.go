package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

func webServerAppLog() *slog.Logger { return slog.Default().With("logger", "hermes_cli.web_server") }

// Startup steps of the standalone lifespan, the real functions swapped only where a hermes case installs a fake
var (
	lifespanWarmGatewayModule = warmGatewayModule
	lifespanEagerReconcile    = eagerReconcileOwnSessionDB
	lifespanStartHostedRooms  = tuigateway.StartHostedRoomService
	lifespanStopHostedRooms   = tuigateway.StopHostedRoomService
	lifespanStartCronTicker   = cron.StartDesktopCronTicker
	lifespanTerminateGateway  = terminateDesktopManagedGateway
	statusRestartDrainTimeout = resolveRestartDrainTimeout
)

// Drops every PTY channel marker left in app state after the registry closed its live sessions
func unlinkPtyMarkers(app *FastAPI) {
	seen := map[string]bool{}
	for _, marker := range getPtyActiveSessionFiles(app).Values() {
		if seen[marker] {
			continue
		}
		seen[marker] = true
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
	}
}

// Running lifespan of one app, what exit must stop, join and release
type appLifespanRun struct {
	cancel            context.CancelFunc
	tasks             sync.WaitGroup
	eagerReconcile    chan struct{}
	hostedStart       chan struct{}
	hostedStartCancel chan struct{}
	cronStop          chan struct{}
	desktopOwned      bool
	httpListener      bool
	hostTerminalExit  func(ctx context.Context) error
}

// Fresh per run app state the chat and PTY routes bind to the running server
func resetLifespanAppState(app *FastAPI) {
	app.State.Set("event_channels", newEventChannels())
	app.State.Set("event_lock", &sync.Mutex{})
	app.State.Set("pty_active_session_files", NewPtySessionFiles())
	app.State.Set("chat_argv_lock", &sync.Mutex{})
}

// Standalone backend services, schema catch up, hosted rooms, desktop cron, PTY reaper, self test and local runtime
func standaloneLifespanEnter(ctx context.Context, app *FastAPI) (*appLifespanRun, error) {
	resetLifespanAppState(app)
	base := context.WithoutCancel(ctx)
	runCtx, cancel := context.WithCancel(base)
	run := &appLifespanRun{cancel: cancel, eagerReconcile: make(chan struct{}), hostedStart: make(chan struct{}),
		hostedStartCancel: make(chan struct{})}
	go func() {
		defer close(run.eagerReconcile)
		lifespanEagerReconcile(base)
	}()
	lifespanWarmGatewayModule(ctx)
	gateway.RecordBootFingerprint()
	tuiserver.InstallTUIMessageInjector(base, nil)
	go func() {
		defer close(run.hostedStart)
		if _, err := lifespanStartHostedRooms(); err != nil {
			webServerAppLog().Error("Hosted Group Chat recovery failed during backend startup", "exc_info", err)
		}
		select {
		case <-run.hostedStartCancel:
			lifespanStopHostedRooms(1.0)
		default:
		}
	}()
	run.desktopOwned = tools.IsDesktopOwnedBackend(nil)
	if run.desktopOwned {
		if _, err := gateway.ReapUnsupervisedGatewayOrphans(ctx, nil, gateway.ReapMinAgeSeconds); err != nil {
			webServerAppLog().Error("Desktop startup: orphan gateway reap failed", "exc_info", err)
		}
		run.cronStop = make(chan struct{})
		go lifespanStartCronTicker(base, run.cronStop, 60)
	}
	ptyRegistry.Reopen()
	run.tasks.Add(3)
	go func() { defer run.tasks.Done(); tools.RunPtyReaper(runCtx, ptyRegistry, 60*time.Second) }()
	go func() { defer run.tasks.Done(); dashboardSelftestLoop(runCtx, app) }()
	go func() { defer run.tasks.Done(); autoArchiveTickerLoop(runCtx, 3600, 90) }()
	go func() {
		cfg, err := config.LoadContext(base, home.Dir(base), config.EnvRefLookupContext(base))
		if err == nil {
			_, err = provider.EnsureLocalRuntime(base, map[string]any(cfg), false)
		}
		if err != nil {
			slog.Default().With("logger", "hermes_cli.web_server_app").Warn("local runtime boot failed: " + err.Error())
		}
	}()
	provider.StartBackgroundBootstrap(bootstrapPoolEnv(base))
	return run, nil
}

// Pool environment of the active profile home with the context secret scope the bootstrap thread inherits
func bootstrapPoolEnv(ctx context.Context) provider.PoolEnv {
	env := provider.HomePoolEnv(home.Dir(ctx))
	env.Scope = provider.SecretScopeFrom(ctx)
	env.Ctx = ctx
	return env
}

// Clears the injector, stops hosted rooms, cron and the tasks, closes PTYs and the runtime, then the desktop gateway
func standaloneLifespanExit(ctx context.Context, app *FastAPI, run *appLifespanRun) error {
	tuiserver.ClearTUIMessageInjector(context.WithoutCancel(ctx), nil)
	close(run.hostedStartCancel)
	lifespanStopHostedRooms(5.0)
	select {
	case <-run.hostedStart:
	case <-time.After(time.Second):
	}
	if run.cronStop != nil {
		close(run.cronStop)
	}
	run.cancel()
	<-run.eagerReconcile
	ptyRegistry.CloseAll(ctx)
	unlinkPtyMarkers(app)
	if err := provider.ShutdownLocalRuntime(); err != nil {
		webServerAppLog().Warn("managed local runtime shutdown failed", "exc_info", err)
	}
	if run.desktopOwned {
		lifespanTerminateGateway()
	}
	return nil
}

// Gateway API listener lifespan, the PTY reaper, the self test and the host terminal when the surface serves one
func httpLifespanEnter(ctx context.Context, app *FastAPI) (*appLifespanRun, error) {
	resetLifespanAppState(app)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &appLifespanRun{cancel: cancel, httpListener: true}
	ptyRegistry.Reopen()
	run.tasks.Add(2)
	go func() { defer run.tasks.Done(); tools.RunPtyReaper(runCtx, ptyRegistry, 60*time.Second) }()
	go func() { defer run.tasks.Done(); dashboardSelftestLoop(runCtx, app) }()
	if PolicyOf(app.State).HostTerminal {
		exit, err := hostTerminalLifespan(ctx, app)
		if err != nil {
			cancel()
			run.tasks.Wait()
			ptyRegistry.CloseAll(ctx)
			unlinkPtyMarkers(app)
			return nil, err
		}
		run.hostTerminalExit = exit
	}
	return run, nil
}

// Leaves the host terminal, cancels and awaits the tasks, closes PTYs and drops their markers
func httpLifespanExit(ctx context.Context, app *FastAPI, run *appLifespanRun) error {
	var err error
	if run.hostTerminalExit != nil {
		err = run.hostTerminalExit(ctx)
	}
	run.cancel()
	run.tasks.Wait()
	ptyRegistry.CloseAll(ctx)
	unlinkPtyMarkers(app)
	return err
}

// Lifespan hooks of the app, the gateway API lifespan when a gateway runner is mounted, the standalone one otherwise
func appLifespan(app func() *FastAPI) (enter, exit func(ctx context.Context) error) {
	var mu sync.Mutex
	var run *appLifespanRun
	enter = func(ctx context.Context) error {
		a := app()
		var r *appLifespanRun
		var err error
		if a.State.Get("gateway_runner") != nil {
			r, err = httpLifespanEnter(ctx, a)
		} else {
			r, err = standaloneLifespanEnter(ctx, a)
		}
		if err != nil {
			return err
		}
		mu.Lock()
		run = r
		mu.Unlock()
		return nil
	}
	exit = func(ctx context.Context) error {
		mu.Lock()
		r := run
		run = nil
		mu.Unlock()
		if r == nil {
			return nil
		}
		if r.httpListener {
			return httpLifespanExit(ctx, app(), r)
		}
		return standaloneLifespanExit(ctx, app(), r)
	}
	return enter, exit
}

// Authenticated plugin API requests refused when the plugin is disabled, else forwarded to its hosting profile
func pluginAPIRuntimeGate(ctx context.Context, req *Request, callNext CallNext) (ASGIResponse, error) {
	path := req.URL().Path()
	name := pluginAPIName(req.Scope)
	if name != "" && (req.State().Bool("token_authenticated") || req.State().Get("native_http_principal") != nil ||
		req.App().State.Bool("auth_required") || hasValidSessionToken(req) || hasValidQueryToken(req, path)) {
		sctx, release, err := pluginRequestScope(ctx, req.Scope)
		if err != nil {
			return nil, err
		}
		if pluginAPIRequestRefused(sctx, name) {
			release()
			return MustJSONResponse(map[string]any{"detail": "Plugin not found"}, 404), nil
		}
		hosted, err := forwardSecondaryPluginRequest(sctx, req, name)
		release()
		if err != nil {
			return nil, err
		}
		if hosted != nil {
			return hosted, nil
		}
	}
	return callNext(ctx, req)
}

// Application of web_server.py, its middlewares and own routes in import order with the routers include adds at L780
func buildWebServerApp(include func(app *FastAPI)) *FastAPI {
	normalizeWebAssetMIMETypes()
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		panic(err)
	}
	var app *FastAPI
	enter, exit := appLifespan(func() *FastAPI { return app })
	app = NewFastAPI("Icebath Agent", info.BaseVersion, enter, exit)
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(app.AddMiddleware(AuthBodyLimitMiddleware))
	app.IncludeRouter(memoryOAuthRouter(), "")
	must(app.AddMiddleware(ChatFileUploadLimitMiddleware))
	must(app.HTTPMiddleware(hostHeaderMiddleware(app)))
	must(app.HTTPMiddleware(pluginAPIRuntimeGate))
	must(app.AddMiddleware(PluginAPIWebSocketGate))
	must(app.HTTPMiddleware(dashboardAuthGate))
	must(app.HTTPMiddleware(authMiddleware))
	must(app.HTTPMiddleware(tokenAuthSeam))
	must(app.HTTPMiddleware(dashboardHealthMiddleware))
	must(app.AddMiddleware(localOriginCORSMiddleware(app)))
	include(app)
	mountPluginAPIRoutes(app)
	app.IncludeRouter(dashboardAuthRouter(), "")
	mountSPA(app)
	return app
}
