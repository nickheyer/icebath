package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds the cron ticker and the housekeeping goroutine may take to exit after shutdown
const (
	cronShutdownDrainTimeout         = 65.0
	housekeepingShutdownDrainTimeout = 35.0
)

// Restores the user proxy env an update restart replaced, assigned from pkg/selfupdate in cmd/icebath wiring
var RestoreUserProxyEnv func(environ map[string]string)

// Arguments of start_gateway, a nil config loading the runner config and a nil verbosity meaning quiet
type StartGatewayOptions struct {
	Config    *gateway.GatewayConfig
	Replace   bool
	Force     bool
	Verbosity *int
	IdleExit  bool
}

// Background goroutine of the bootstrap with a done signal, the daemon thread of hermes
type bootThread struct{ done chan struct{} }

// Runs fn on its own goroutine
func startBootThread(fn func()) *bootThread {
	t := &bootThread{done: make(chan struct{})}
	go func() {
		defer close(t.done)
		fn()
	}()
	return t
}

// Whether the goroutine is still running
func (t *bootThread) alive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Waits up to timeout seconds for the goroutine to exit without blocking the loop, true when it exited
func awaitThreadExit(ctx context.Context, alive func() bool, timeout float64) bool {
	if alive == nil {
		return true
	}
	deadline := time.Now().Add(time.Duration(max(0, timeout) * float64(time.Second)))
	for alive() && time.Now().Before(deadline) {
		if sleepCtx(ctx, 0.1) != nil {
			break
		}
	}
	return !alive()
}

// Builds the interrupt and terminate handler that records whether an unplanned signal began the shutdown
func startGatewayMakeShutdownSignalHandler(ctx context.Context, r *GatewayRunner, signalInitiated *bool) func(sig *int) {
	plannedStopSeen := false
	return func(sig *int) {
		takeover, _ := bestEffort(gateway.ConsumeTakeoverMarkerForSelf, "Takeover marker check failed: %s")
		plannedStop := sig != nil && *sig == int(syscall.SIGINT)
		if !plannedStop && !takeover {
			plannedStop, _ = bestEffort(gateway.ConsumePlannedStopMarkerForSelf, "Planned stop marker check failed: %s")
		}
		if plannedStop {
			plannedStopSeen = true
		} else if plannedStopSeen && !takeover {
			plannedStop = true
		}
		shutdownCtx, _ := bestEffort(func() (*pyjson.Object, error) { return gateway.SnapshotShutdownContext(sig), nil }, "snapshot_shutdown_context failed: %s")
		sigName := ""
		if shutdownCtx != nil {
			if v, ok := shutdownCtx.Get("signal"); ok && v != nil {
				sigName = fmt.Sprint(v)
			}
		}
		orDefault := func(fallback string) string {
			if sigName != "" {
				return sigName
			}
			return fallback
		}
		switch {
		case takeover:
			runLog().Info(fmt.Sprintf("Received %s as a planned --replace takeover — exiting cleanly", orDefault("SIGTERM")))
		case plannedStop:
			runLog().Info(fmt.Sprintf("Received %s as a planned gateway stop — exiting cleanly", orDefault("SIGTERM/SIGINT")))
		default:
			*signalInitiated = true
			r.signalInitiatedShutdown = true
			runLog().Info(fmt.Sprintf("Received %s — initiating shutdown", orDefault("SIGTERM/SIGINT")))
		}
		if shutdownCtx != nil {
			bestEffort(func() (struct{}, error) {
				runLog().Warn(fmt.Sprintf("Shutdown context: %s", gateway.FormatContextForLog(shutdownCtx)))
				return struct{}{}, nil
			}, "format_context_for_log failed: %s")
			bestEffort(func() (struct{}, error) {
				gateway.SpawnAsyncDiagnostic(filepath.Join(home.Process(), "logs", "gateway-shutdown-diag.log"), sigName, 5.0)
				return struct{}{}, nil
			}, "spawn_async_diagnostic failed: %s")
		}
		if !takeover {
			r.stopRequestedBySignal = true
		}
		spawnGatewayTask(context.WithoutCancel(ctx), "", func(c context.Context) (any, error) {
			return nil, r.Stop(c, false, false, false)
		})
	}
}

// Builds the restart signal handler that logs its meaning then requests the drain aware service restart
func startGatewayMakeRestartSignalHandler(r *GatewayRunner) func() {
	return func() {
		runLog().Info("SIGUSR1 received (systemctl reload / icebath gateway restart): performing a graceful " +
			"gateway restart — drain active turns, exit, supervisor relaunches. Not an in-process " +
			"config reload.")
		r.RequestRestart(false, true)
	}
}

// Connects the MCP servers of every served profile with interactive OAuth suppressed
func discoverGatewayMCPTools(ctx context.Context, cfg *gateway.GatewayConfig) error {
	ctx = mcp.SuppressInteractiveOAuth(ctx)
	if cfg == nil || cfg.MultiplexProfiles == nil || !*cfg.MultiplexProfiles {
		_, err := mcp.ConnectAll(ctx, &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, nil)
		return err
	}
	served, err := gateway.MultiplexProfileHomes(ctx, cfg)
	if err != nil {
		return err
	}
	for _, p := range served {
		scoped := gateway.ProfileRuntimeScope(ctx, p.Home, true)
		if _, err := mcp.ConnectAll(scoped, &tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)}, nil); err != nil {
			runLog().Warn(fmt.Sprintf("MCP tool discovery failed for profile '%s'", p.Name), "exc_info", err)
		}
	}
	return nil
}

// Closes the MCP servers per served profile then the shared loop in the background, true when done within timeout
func shutdownMCPServersNonblocking(ctx context.Context, timeout float64, cfg *gateway.GatewayConfig) (bool, error) {
	var served []struct{ Name, Home string }
	if cfg != nil && cfg.MultiplexProfiles != nil && *cfg.MultiplexProfiles {
		homes, err := gateway.MultiplexProfileHomes(ctx, cfg)
		if err != nil {
			return false, err
		}
		for _, p := range homes {
			served = append(served, struct{ Name, Home string }{p.Name, p.Home})
		}
	}
	passTimeout := max(1.0, timeout/float64(len(served)+1))
	pass := time.Duration(passTimeout * float64(time.Second))
	worker := startBootThread(func() {
		for _, p := range served {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						runLog().Debug(fmt.Sprintf("MCP shutdown raised for profile '%s'", p.Name), "exc_info", rec)
					}
				}()
				scoped := gateway.ProfileRuntimeScope(context.Background(), p.Home, false)
				scope := mcp.RegistryScope(scoped)
				mcp.ShutdownMCPServers(scoped, &scope, nil, pass)
			}()
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("MCP shutdown raised", "exc_info", rec)
				}
			}()
			mcp.ShutdownMCPServers(provider.WithLaunchProfileScopeIfMultiplexed(context.Background()), nil, nil, pass)
		}()
	})
	done := awaitThreadExit(ctx, worker.alive, timeout)
	if !done {
		runLog().Warn(fmt.Sprintf("MCP shutdown did not finish within %.1fs; continuing gateway "+
			"teardown (background thread will be reaped at process exit)", timeout))
	}
	return done, nil
}

// Stops a cron provider without letting it choose the gateway exit code
func stopCronProvider(p cron.CronScheduler) {
	defer func() {
		if rec := recover(); rec != nil {
			var exit *gateway.SystemExitError
			if err, ok := rec.(error); ok && errors.As(err, &exit) {
				runLog().Warn(fmt.Sprintf("Cron provider stop() attempted to exit the gateway with code %d; ignoring", exit.Code))
				return
			}
			runLog().Debug(fmt.Sprintf("Cron provider stop() error: %v", rec))
		}
	}()
	cron.SchedulerStop(p)
}

// MCP discovery then the runner start, the exit verdict when the process must end before running mode
func startGatewayRunRunner(ctx context.Context, r *GatewayRunner, signalInitiated *bool) (*bool, error) {
	if err := discoverGatewayMCPTools(ctx, r.config); err != nil {
		runLog().Debug("MCP tool discovery failed (non-fatal)", "exc_info", err)
	}
	success, err := r.Start(ctx)
	if err != nil {
		shutdownGatewayHealthExport(r)
		return nil, err
	}
	if !success {
		shutdownGatewayHealthExport(r)
		return new(false), nil
	}
	if r.exitCleanly {
		shutdownGatewayHealthExport(r)
		if r.exitReason != nil && *r.exitReason != "" {
			runLog().Error(fmt.Sprintf("Gateway exiting cleanly: %s", *r.exitReason))
		}
		if r.exitCode != nil {
			return nil, &gateway.SystemExitError{Code: *r.exitCode}
		}
		return new(true), nil
	}
	if !r.running.Load() {
		defer shutdownGatewayHealthExport(r)
		if err := r.WaitForShutdown(ctx); err != nil {
			return nil, err
		}
		if _, err := shutdownMCPServersNonblocking(ctx, 5.0, r.config); err != nil {
			runLog().Warn("MCP shutdown failed; connections may be left open", "exc_info", err)
		}
		verdict, err := resolveGatewayExitVerdict(r, *signalInitiated)
		if err != nil {
			return nil, err
		}
		return &verdict, nil
	}
	return nil, nil
}

// Cron and housekeeping goroutines of a running gateway with the stop event they share
type cronHousekeeping struct {
	stop         *pythread.Event
	provider     cron.CronScheduler
	cronThread   *cron.SupervisedTickerThread
	housekeeping *bootThread
}

// Teardown after the runner stopped, returning the process exit verdict
func startGatewayShutdownTail(ctx context.Context, r *GatewayRunner, control *gateway.GatewayControlServer, ch cronHousekeeping,
	plannedStop *pythread.Event, plannedStopThread *bootThread, signalInitiated *bool) (bool, error) {
	if control != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Control socket stop failed (non-fatal)", "exc_info", rec)
				}
			}()
			control.Stop()
		}()
	}
	bestEffort(func() (struct{}, error) { provider.StopNousAuthKeepalive(0); return struct{}{}, nil }, "")
	ch.stop.Set()
	stopCronProvider(ch.provider)
	if !awaitThreadExit(ctx, ch.cronThread.IsAlive, cronShutdownDrainTimeout) {
		runLog().Warn(fmt.Sprintf("Cron ticker did not exit within %.0fs of shutdown — an in-flight "+
			"delivery may have been dropped.", cronShutdownDrainTimeout))
	}
	awaitThreadExit(ctx, ch.housekeeping.alive, housekeepingShutdownDrainTimeout)
	plannedStop.Set()
	select {
	case <-plannedStopThread.done:
	case <-time.After(2 * time.Second):
	}
	if _, err := shutdownMCPServersNonblocking(ctx, 5.0, r.config); err != nil {
		runLog().Warn("MCP shutdown failed; connections may be left open", "exc_info", err)
	}
	return resolveGatewayExitVerdict(r, *signalInitiated)
}

// Starts the cron ticker and the housekeeping goroutine sharing one stop event
func startGatewayStartCronAndHousekeeping(ctx context.Context, r *GatewayRunner) cronHousekeeping {
	stop := &pythread.Event{}
	homes, err := cronTickProfileHomes(ctx, r.config)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Could not resolve profile homes for cron: %v", err))
		homes = nil
	}
	h := home.Dir(ctx)
	p := cron.SchedulerForProfileMode(h, cron.ResolveCronScheduler(h), len(homes) > 1)
	opts := cron.StartOptions{Adapters: r.adapters, Loop: r.gatewayLoop}
	_, inProcess := p.(*cron.InProcessCronScheduler)
	if inProcess && len(homes) > 0 {
		opts.ProfileHomesFn = func() ([]cron.ProfileHome, error) {
			served, err := cronTickProfileHomes(ctx, r.config)
			if err != nil {
				return nil, err
			}
			out := make([]cron.ProfileHome, 0, len(served))
			for _, s := range served {
				name := s.Name
				out = append(out, cron.ProfileHome{Name: &name, Home: s.Home})
			}
			return out, nil
		}
		opts.ProfileGate = func(name *string, profileHome string) bool {
			n := ""
			if name != nil {
				n = *name
			}
			return cronProfileGate(n, profileHome)
		}
		opts.ProfileAdapters = map[string]any{}
		for name, m := range r.profileAdapters.All() {
			opts.ProfileAdapters[name] = m
		}
		opts.ProfileBotPlatforms = map[string]any{}
		for name, m := range r.profileBotPlatforms {
			opts.ProfileBotPlatforms[name] = m
		}
		opts.DefaultProfile = &r.primaryProfileName
		names := make([]string, 0, len(homes))
		for _, s := range homes {
			names = append(names, s.Name)
		}
		runLog().Info(fmt.Sprintf("Cron scheduler will tick %d profile(s): %s", len(homes), pyReprStrings(names)))
	}
	if inProcess {
		opts.CanDispatch = func() bool { return !(r.draining.Load() || r.externalDrainActive) }
	}
	stopCh := make(chan struct{})
	go func() {
		stop.Wait(-1)
		close(stopCh)
	}()
	cronThread := cron.NewSupervisedTickerThread(func() { p.Start(context.WithoutCancel(ctx), stopCh, opts) }, stopCh, "cron-ticker")
	gateway.ProcessOwnership.StartWriterThread(cronThread.IsAlive, func() { cronThread.Start() })
	if !inProcess {
		if _, hasAPIServer := r.adapters.Get(gateway.PlatformAPIServer); !hasAPIServer {
			runLog().Warn(fmt.Sprintf("Cron provider '%s' is active but the api_server adapter is "+
				"NOT running in this gateway — scheduled fires arrive over "+
				"loopback HTTP and will all fail (jobs only run when "+
				"triggered manually). Most common cause: API_SERVER_KEY is "+
				"missing from this gateway process's environment. Restart "+
				"the gateway through its supervisor (`icebath gateway "+
				"restart`) so the profile env loads.", p.Name()))
		}
	}
	housekeeping := &bootThread{done: make(chan struct{})}
	gateway.ProcessOwnership.StartWriterThread(housekeeping.alive, func() {
		go func() {
			defer close(housekeeping.done)
			startGatewayHousekeeping(context.WithoutCancel(ctx), stop, housekeepingArgs{
				adapters: r.adapters, loop: r.gatewayLoop, cronProvider: p, runner: r, cronThread: cronThread,
			})
		}()
	})
	return cronHousekeeping{stop: stop, provider: p, cronThread: cronThread, housekeeping: housekeeping}
}

// Seams of the gateway start, the real functions swapped only where a hermes case installs a fake
var (
	startCronAndHousekeeping = startGatewayStartCronAndHousekeeping
	publishRuntimeReady      = publishGatewayRuntimeReady
)

// Starts the gateway and runs until interrupted, false when it failed to start so supervisors restart it
func StartGateway(ctx context.Context, o StartGatewayOptions) (bool, error) {
	os.Setenv("ICEBATH_EXEC_ASK", "1")
	RestoreUserProxyEnv(nil)
	if err := startGatewayResolveTerminalCwd(); err != nil {
		return false, err
	}
	gateway.ApplyNofileSoftLimit(nil)
	gateway.RecordBootFingerprint()
	resolved := o.Config
	if resolved == nil {
		loaded, err := loadGatewayConfigForRunner(ctx)
		if err != nil {
			return false, err
		}
		resolved = loaded
	}
	startGatewayEarlyEnvironmentProbe(ctx)
	var profileHomes []profiles.ServedProfile
	if resolved.MultiplexProfiles != nil && *resolved.MultiplexProfiles {
		homes, err := gateway.MultiplexProfileHomes(ctx, resolved)
		if err != nil {
			return false, err
		}
		profileHomes = homes
	}
	if len(profileHomes) > 0 {
		may, err := launchHomeMayMultiplex(ctx, resolved)
		if err != nil {
			return false, err
		}
		if !may {
			return false, nil
		}
		if resolved.MultiplexProfiles == nil || !*resolved.MultiplexProfiles {
			profileHomes = nil
		}
	}
	decision, err := hostAttachOrNone(ctx, o.Replace, o.Force)
	if err != nil {
		return false, err
	}
	if decision != nil {
		return *decision, nil
	}
	existing, running, err := gateway.GetRunningPid("", true)
	if err != nil {
		return false, err
	}
	if running && existing != os.Getpid() {
		replaced, err := startGatewayReplaceExistingInstance(ctx, existing, o.Replace)
		if err != nil {
			return false, err
		}
		if !replaced {
			return false, nil
		}
	}
	reserved, err := startGatewayReserveProfiles(ctx, profileHomes)
	if err != nil {
		return false, err
	}
	if !reserved {
		return false, nil
	}
	if len(profileHomes) > 0 {
		resolved.SetRuntimeProfileHomes(profileHomes)
	}
	claimed, err := startGatewayClaimPidFile(ctx, o.Force)
	if err != nil {
		return false, err
	}
	if !claimed {
		gateway.ReleaseGatewayRuntimeLock()
		return false, nil
	}
	return runStartedGateway(ctx, o, resolved)
}

// Body of start_gateway after the pid claim, with the cleanup that always drains, stops and releases the claim
func runStartedGateway(ctx context.Context, o StartGatewayOptions, resolved *gateway.GatewayConfig) (verdict bool, err error) {
	var control *gateway.GatewayControlServer
	var r *GatewayRunner
	var plannedStop *pythread.Event
	loop := gateway.NewCallbackLoop()
	settled := context.WithoutCancel(ctx)
	loopCtx, stopLoop := context.WithCancel(settled)
	go loop.Run(loopCtx)
	stopSignals := func() {}
	defer func() {
		defer func() {
			if plannedStop != nil {
				plannedStop.Set()
			}
			if control != nil {
				control.Stop()
			}
			gateway.RemovePidFile()
			gateway.ReleaseGatewayRuntimeLock()
			taskExceptionLoop.CompareAndSwap(loop, nil)
			loop.Close()
			stopLoop()
			stopSignals()
		}()
		if r != nil {
			r.drainGatewayRuntime(settled)
			if r.currentStopTask() == nil && !r.running.Load() {
				r.startupCleanupOnly = true
			}
			stopErr := r.Stop(settled, false, false, false)
			if err == nil {
				err = stopErr
			}
		}
	}()
	if err := startGatewayConfigureLogging(ctx, o.Verbosity); err != nil {
		return false, err
	}
	if err := recoverLeftCoreAtGatewayStart(ctx); err != nil {
		return false, err
	}
	pin := pinLaunchStore(ctx)
	r, err = func() (*GatewayRunner, error) {
		defer unpinLaunchStore(pin)
		return NewGatewayRunner(ctx, resolved)
	}()
	if err != nil {
		return false, err
	}
	r.gatewayLoop = loop
	if err := r.initializeGatewayRuntime(ctx); err != nil {
		return false, err
	}
	enableMultiplexLogRouting(ctx, r.config)
	r.platformLockTakeoverOnStart = o.Replace
	signalInitiated := false
	shutdownHandler := startGatewayMakeShutdownSignalHandler(ctx, r, &signalInitiated)
	restartHandler := startGatewayMakeRestartSignalHandler(r)
	loop.SetExceptionHandler(gatewayLoopExceptionHandler)
	taskExceptionLoop.Store(loop)
	stopSignals = startGatewayInstallSignalHandlers(loop, shutdownHandler, restartHandler)
	plannedStop = &pythread.Event{}
	plannedStopThread := startBootThread(func() { runPlannedStopWatcher(plannedStop, r, loop, shutdownHandler, 1.0) })
	control = startGatewayStartControlSocket(ctx, r, loop)
	if control == nil {
		return false, errors.New("gateway session bootstrap control listener unavailable")
	}
	refreshHostGatewayRecord(ctx, r)
	logStandaloneProfilesAtBoot(ctx, r)
	if err := startGatewayRuntimeAPI(ctx, r); err != nil {
		return false, err
	}
	unclean, _ := bestEffort(func() (*pyjson.Object, error) { return gateway.RecordStartup(ctx, "", true), nil }, "Lifecycle ledger startup record failed: %s")
	bestEffort(func() (bool, error) {
		return provider.StartNousAuthKeepalive(provider.HomePoolEnv(home.Dir(ctx)), provider.NousKeepaliveStartOptions{}), nil
	}, "Nous auth keepalive did not start: %s")
	early, err := startGatewayRunRunner(ctx, r, &signalInitiated)
	if err != nil {
		return false, err
	}
	if early != nil {
		return *early, nil
	}
	if unclean != nil {
		startDeferredStateDBIntegrityCheck(ctx, unclean)
	}
	ch := startCronAndHousekeeping(ctx, r)
	waitErr := func() error {
		if err := publishRuntimeReady(r); err != nil {
			return err
		}
		r.startSystemdWatchdog(ctx)
		if o.IdleExit {
			armUnmanagedIdleExit(r)
		} else {
			warmGatewayDashboard(ctx, r)
		}
		return waitGatewayRuntime(ctx, r)
	}()
	stopErr := r.Stop(settled, false, false, false)
	verdict, tailErr := startGatewayShutdownTail(settled, r, control, ch, plannedStop, plannedStopThread, &signalInitiated)
	if err := errors.Join(waitErr, stopErr, tailErr); err != nil {
		return false, err
	}
	return verdict, nil
}

// Python repr of a list of strings, the way a profile name list prints in a log line
func pyReprStrings(items []string) string {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return pyrepr.Repr(out)
}
