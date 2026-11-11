package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Logs the enforced per turn iteration budget the bridged env slot resolves to
func logAgentBudget() {
	var raw any
	if v, ok := os.LookupEnv("ICEBATH_MAX_ITERATIONS"); ok {
		raw = v
	}
	limit := config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
	shown := any(limit)
	if limit == config.TurnLimitUnlimited {
		shown = "unlimited"
	}
	runLog().Info(fmt.Sprintf("Agent budget: max_iterations=%v (agent.max_turns from config.yaml, else the ICEBATH_MAX_ITERATIONS bridge)", shown))
}

// Parks the last platform after a non retryable failure so the gateway stays up degraded, false when it must stop
func (r *GatewayRunner) parkLastFatalPlatform(ctx context.Context, adapter gateway.PlatformAdapter) bool {
	base := adapter.Base()
	code := ""
	if c := base.FatalErrorCode(); c != nil {
		code = *c
	}
	if base.FatalErrorRetryable() || gateway.IsGlobalStartupConflict(code) {
		return false
	}
	r.startupParkedPlatforms = true
	if !(r.externalDrainActive || r.draining.Load() || !r.running.Load()) {
		state := r.servingState()
		r.updateRuntimeStatus(ctx, &state, nil)
	}
	shownCode, shownMessage := "unknown", "unknown error"
	if code != "" {
		shownCode = code
	}
	if m := base.FatalErrorMessage(); m != nil && *m != "" {
		shownMessage = *m
	}
	runLog().Error(fmt.Sprintf("%s is parked after a non-retryable error (fix it, then `icebath gateway restart`). The gateway "+
		"is DEGRADED — it serves local clients and cron with no messaging platform. %s: %s",
		base.Platform, shownCode, shownMessage))
	return true
}

// Arms the loop floor timer and the out of loop liveness watchdog unless the config turns the watchdog off
func (r *GatewayRunner) startLoopLivenessGuards(ctx context.Context, loop gateway.EventLoop) {
	if r.config != nil && !r.config.LoopWatchdog {
		return
	}
	if r.loopFloorTimerHandle == nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Failed to arm gateway loop floor timer", "exc_info", rec)
				}
			}()
			handle, err := gateway.ArmLoopFloorTimer(loop, nil)
			if err != nil {
				runLog().Debug("Failed to arm gateway loop floor timer", "exc_info", err)
				return
			}
			r.loopFloorTimerHandle = handle
		}()
	}
	if watchdog := r.loopLivenessWatchdog; watchdog == nil || !watchdog.IsAlive() {
		interval, timeout := gateway.DefaultLoopWatchdogIntervalS, gateway.DefaultLoopWatchdogTimeoutS
		strikes := gateway.DefaultLoopWatchdogMaxStrikes
		if r.config != nil {
			interval, timeout = r.config.LoopWatchdogProbeIntervalS, r.config.LoopWatchdogProbeTimeoutS
			strikes = r.config.LoopWatchdogMaxStrikes
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("Failed to start gateway loop liveness watchdog", "exc_info", rec)
				}
			}()
			r.loopLivenessWatchdog = gateway.StartLoopLivenessWatchdog(ctx, loop, interval, timeout, strikes, gateway.GatewayServiceRestartExitCode)
		}()
	}
}

// Starts the heartbeat task once, tagged as a permanent watcher and tracked with the background tasks
func (r *GatewayRunner) startLoopHeartbeatTask(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug("Failed to start gateway loop heartbeat", "exc_info", rec)
		}
	}()
	if existing := r.loopHeartbeatTask; existing != nil && !existing.isDone() {
		return
	}
	startTime := r.gatewayStartedAt
	loop := r.gatewayLoop
	task := spawnGatewayTask(context.WithoutCancel(ctx), "gateway-loop-heartbeat", func(taskCtx context.Context) (any, error) {
		return nil, gateway.LoopHeartbeatForever(taskCtx, loop, gateway.LoopHeartbeatOptions{
			IntervalS: gateway.DefaultHeartbeatIntervalS, StartTime: &startTime,
		})
	})
	r.loopHeartbeatTask = task
	r.taskSetsMu.Lock()
	if r.supervisedWatchers == nil {
		r.supervisedWatchers = map[*gatewayTask]struct{}{}
	}
	r.supervisedWatchers[task] = struct{}{}
	bg := r.backgroundTasks
	r.taskSetsMu.Unlock()
	if bg != nil {
		r.trackTaskIn(bg, task)
	}
}

// Opens the faulthandler log under the home logs directory for append, creating the directory
func openFaulthandlerLog(ctx context.Context) (*os.File, error) {
	logDir := filepath.Join(home.Dir(ctx), "logs")
	if err := os.MkdirAll(logDir, 0o777); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(logDir, "gateway_faulthandler.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
}

// Installs the crash and dump signal goroutine dumps, the crash dump going to the log file without stderr
func (r *GatewayRunner) startInstallFaulthandler(ctx context.Context) {
	debug.SetTraceback("all")
	if _, err := os.Stderr.Stat(); err != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug("faulthandler.enable() unavailable", "exc_info", rec)
				}
			}()
			f, err := openFaulthandlerLog(ctx)
			if err == nil {
				err = debug.SetCrashOutput(f, debug.CrashOptions{})
				f.Close()
			}
			if err != nil {
				runLog().Debug("faulthandler.enable() unavailable", "exc_info", err)
			}
		}()
	}
	if stackDumpSignal == nil {
		return
	}
	f, err := openFaulthandlerLog(ctx)
	if err != nil {
		runLog().Debug("Could not set up faulthandler file logging", "exc_info", err)
		return
	}
	registerStackDump(f)
}

// Every goroutine stack, the all threads dump of faulthandler
func goroutineStacks() []byte {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// Warns per served profile whose config still declares a timed session reset unless the restoring plugin is enabled
func (r *GatewayRunner) startLogRetiredSessionReset(ctx context.Context) {
	type hit struct{ name, path, mode string }
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		served, err := profiles.ProfilesToServe(ctx, r.multiplexing(), false, false)
		if err != nil {
			return err
		}
		var hits []hit
		for _, p := range served {
			layers, err := gateway.ReadYAMLLayers(ctx, p.Home)
			if err != nil {
				return err
			}
			if path, mode, ok := plugins.RetiredResetPolicy(layers); ok {
				hits = append(hits, hit{p.Name, path, mode})
			}
		}
		if len(hits) > 0 && !plugins.ResetPluginEnabled(ctx) {
			for _, h := range hits {
				runLog().Warn(fmt.Sprintf("Profile %s: %s", h.name, plugins.FormatSessionResetNotice(h.path, h.mode)))
			}
		}
		return nil
	}()
	if err != nil {
		runLog().Debug("retired session_reset check failed", "exc_info", err)
	}
}

// Warns when the systemd stop timeout of the unit does not cover the drain window
func (r *GatewayRunner) startLogSystemdTimingAlignment(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			logSuppressed(slog.LevelDebug, fmt.Errorf("%v", rec), "check_systemd_timing_alignment failed: %s")
		}
	}()
	alignment, err := gateway.CheckSystemdTimingAlignment(ctx, r.restartDrainTimeout, r.cronDrainTimeout)
	if err != nil {
		logSuppressed(slog.LevelDebug, err, "check_systemd_timing_alignment failed: %s")
		return
	}
	if alignment == nil {
		return
	}
	if mismatch, _ := alignment.Get("mismatch"); !pyrepr.Truthy(mismatch) {
		return
	}
	f0 := func(v any) string {
		text, err := pyrepr.Format(v, ".0f")
		if err != nil {
			panic(err)
		}
		return text
	}
	must := func(o *pyjson.Object, key string) any {
		v, ok := o.Get(key)
		if !ok {
			panic(fmt.Sprintf("KeyError: %s", pyrepr.Repr(key)))
		}
		return v
	}
	runLog().Warn(fmt.Sprintf("Stale systemd unit detected: %s has TimeoutStopSec=%ss but drain_timeout=%ss "+
		"cron_drain_timeout=%ss (expected >=%ss). systemd may SIGKILL the gateway "+
		"mid-drain. Run `icebath gateway install --force` to regenerate the unit, or shorten "+
		"agent.restart_drain_timeout / agent.cron_drain_timeout.",
		pyrepr.Str(alignment.GetOr("unit", "(unknown)")), f0(must(alignment, "timeout_stop_sec")),
		f0(must(alignment, "drain_timeout")), f0(alignment.GetOr("cron_drain_timeout", gateway.DefaultGatewayCronDrainTimeout)),
		f0(must(alignment, "expected_min"))))
}

// Registers the launch profile hooks block, under its own runtime scope when multiplexing
func registerLaunchProfileConfigHooks(ctx context.Context) {
	const failure = "shell-hook/webhook registration failed at gateway startup"
	if !provider.IsMultiplexActive(ctx) {
		gateway.RegisterWorkerConfigHooks(ctx, failure, slog.LevelWarn)
		return
	}
	gateway.RegisterWorkerConfigHooks(gateway.ProfileRuntimeScope(ctx, gateway.ProcessHome(), true), failure, slog.LevelWarn)
}
