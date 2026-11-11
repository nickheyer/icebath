package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Icebath argv for update and restart, this install's own binary, else icebath on PATH, else nil
func resolveHermesBin() []string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return []string{exe}
	}
	if bin, ok := pyshutil.Which("icebath", nil); ok && bin != "" {
		return []string{bin}
	}
	return nil
}

// Restart watcher environment without the gateway guard, the host child env for a multiplexer or the default root
func restartWatcherEnv(ctx context.Context) (map[string]string, error) {
	current := home.Dir(ctx)
	onDefault := func() (same bool) {
		defer func() {
			if recover() != nil {
				same = false
			}
		}()
		a, errA := filepath.EvalSymlinks(current)
		b, errB := filepath.EvalSymlinks(home.Root())
		if errA != nil {
			a = filepath.Clean(current)
		}
		if errB != nil {
			b = filepath.Clean(home.Root())
		}
		return a == b
	}()
	settledMultiplex := provider.IsMultiplexActive(ctx)
	multiplex := false
	if !onDefault && !settledMultiplex {
		func() {
			defer func() {
				if recover() != nil {
					multiplex = false
				}
			}()
			record := gateway.ReadHostRecord(gateway.RoleGateway, false)
			if record != nil && gateway.RecordNamesLiveOwner(record) && len(record.Profiles) > 1 {
				multiplex = true
			}
		}()
	}
	if !onDefault && !settledMultiplex && !multiplex {
		if cfg, err := gateway.LoadGatewayConfig(ctx); err == nil && cfg != nil && cfg.MultiplexProfiles != nil {
			multiplex = *cfg.MultiplexProfiles
		}
	}
	var env map[string]string
	var err error
	if onDefault || settledMultiplex || multiplex {
		env, err = tools.HostGatewayChildEnv(ctx, nil, nil)
	} else {
		env, err = tools.ServedProfileChildEnv(ctx, nil, nil, current, true)
	}
	if err != nil {
		return nil, err
	}
	env = gateway.DropBridgedEnv(env)
	delete(env, "_ICEBATH_GATEWAY")
	return env, nil
}

// KEY=value list of an environment map
func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Spawns the detached Windows watcher, retrying once without job breakaway
func spawnWindowsRestartWatcher(ctx context.Context, hermesCmd []string, currentPID int, restartAfterS float64) error {
	watcherEnv, err := restartWatcherEnv(ctx)
	if err != nil {
		return err
	}
	if watcherEnv["VIRTUAL_ENV"] == "" {
		if inherited := os.Getenv("VIRTUAL_ENV"); inherited != "" {
			watcherEnv["VIRTUAL_ENV"] = inherited
		}
	}
	watcherBinary, err := os.Executable()
	if err != nil {
		return err
	}
	argv := append([]string{watcherBinary, gateway.GatewayDetachedRestartWatcherEntry, fmt.Sprint(currentPID),
		pyrepr.Str(restartAfterS)}, hermesCmd...)
	argv = append(argv, "gateway", "restart")
	start := func(breakaway bool) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = envList(watcherEnv)
		cmd.SysProcAttr = home.DetachAttr(breakaway)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	if err := start(true); err == nil {
		return nil
	}
	if err := start(false); err != nil {
		label, code := "errno", any(nil)
		var errno syscall.Errno
		if errors.As(err, &errno) {
			code = int(errno)
			if runtime.GOOS == "windows" {
				label = "winerror"
			}
		}
		runLog().Warn(fmt.Sprintf("Detached restart watcher was not started after the "+
			"no-breakaway retry (%s; %s=%s). The gateway will not "+
			"be respawned by this restart attempt.", filepath.Base(watcherBinary), label, pyrepr.Repr(code)))
	}
	return nil
}

// Launches the detached helper that runs the gateway restart once this process exits or the wait runs out
func (r *GatewayRunner) launchDetachedRestartCommand(ctx context.Context) error {
	hermesCmd := resolveHermesBin()
	if len(hermesCmd) == 0 {
		runLog().Error("Could not locate icebath binary for detached /restart")
		return nil
	}
	if r.detachedRestartHelperStarted {
		return nil
	}
	r.detachedRestartHelperStarted = true
	currentPID := os.Getpid()
	restartAfterS := max(r.restartDrainTimeout+5.0, 5.0)
	if runtime.GOOS == "windows" {
		return spawnWindowsRestartWatcher(ctx, hermesCmd, currentPID, restartAfterS)
	}
	quoted := make([]string, len(hermesCmd))
	for i, part := range hermesCmd {
		quoted[i] = pyshlex.Quote(part)
	}
	shellCmd := fmt.Sprintf("deadline=$(( $(date +%%s) + %d )); "+
		"while kill -0 %d 2>/dev/null && [ $(date +%%s) -lt $deadline ]; do sleep 0.2; done; "+
		"%s gateway restart", int(restartAfterS), currentPID, strings.Join(quoted, " "))
	argv := []string{"bash", "-lc", shellCmd}
	if setsid, ok := pyshutil.Which("setsid", nil); ok && setsid != "" {
		argv = append([]string{setsid}, argv...)
	}
	watcherEnv, err := restartWatcherEnv(ctx)
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = envList(watcherEnv)
	cmd.SysProcAttr = home.DetachAttr(false)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Chat agents idle past the gateway timeout plus wedged cron runs, the units the restart wait may skip
func (r *GatewayRunner) wedgedAgentCount(ctx context.Context) int {
	return r.wedgedChatAgentCount() + r.wedgedCronJobCount(ctx)
}

// Per run split of in flight cron work, everything awaitable when the split cannot be read
func (r *GatewayRunner) restartWaitCronCounts(ctx context.Context) (counts cron.RestartWaitCronCounts) {
	defer func() {
		if rec := recover(); rec != nil {
			counts = cron.RestartWaitCronCounts{Awaitable: r.activeCronJobCount()}
		}
	}()
	return cron.GetRestartWaitCronCounts(ctx)
}

// Cron runs past their in flight allowance
func (r *GatewayRunner) wedgedCronJobCount(ctx context.Context) int {
	return r.restartWaitCronCounts(ctx).Wedged
}

// Cron runs whose worker owns a restart safe scope outside the gateway cgroup
func (r *GatewayRunner) restartSafeCronCount(ctx context.Context) int {
	return r.restartWaitCronCounts(ctx).RestartSafe
}

// Running chat agents with no activity for the gateway timeout, zero when the timeout is disabled
func (r *GatewayRunner) wedgedChatAgentCount() int {
	timeout := floatEnv("ICEBATH_AGENT_TIMEOUT", 1800)
	if timeout <= 0 {
		return 0
	}
	idleSeconds := func(a *agent.AIAgent) (idle float64, ok bool) {
		defer func() {
			if recover() != nil {
				idle, ok = 0, false
			}
		}()
		summary := a.GetActivitySummary()
		if summary == nil {
			return 0, false
		}
		raw, found := summary["seconds_since_activity"]
		if !found {
			return 0, true
		}
		switch v := raw.(type) {
		case float64:
			return v, true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		}
		return 0, false
	}
	n := 0
	for _, item := range r.runningAgentItems() {
		if item.agent == nil || item.agent == agentPendingSentinel {
			continue
		}
		if idle, ok := idleSeconds(item.agent); ok && idle >= timeout {
			n++
		}
	}
	return n
}

// Active work minus wedged chat turns and restart safe cron runs
func (r *GatewayRunner) awaitableWorkCount(ctx context.Context) int {
	nonCron := r.runningAgentCount() + gateway.ManagedTurnCount(r) + r.activeAPIRunCount() + r.activeDeferredAgentWorkerCount()
	return max(0, nonCron-r.wedgedChatAgentCount()) + r.restartWaitCronCounts(ctx).Awaitable
}

// Waits for in flight work before stop, true when it drained to zero and false when the cap elapsed or only excluded work remains
func (r *GatewayRunner) awaitActiveWorkBeforeRestart(ctx context.Context) (bool, error) {
	active := r.activeWorkCount()
	if active <= 0 {
		return true, nil
	}
	if r.awaitableWorkCount(ctx) <= 0 {
		runLog().Warn(fmt.Sprintf("Restart requested with %d active work unit(s), none awaitable "+
			"(%d wedged past the inactivity timeout, %d in restart-safe external cron "+
			"workers that outlive this process); skipping the after-turn wait and "+
			"proceeding to stop()/drain", active, r.wedgedAgentCount(ctx), r.restartSafeCronCount(ctx)))
		return false, nil
	}
	timeout := r.restartAfterTurnTimeout
	if timeout <= 0 {
		runLog().Info(fmt.Sprintf("Restart requested with %d active work unit(s); "+
			"restart_after_turn_timeout=0 — entering stop()/drain immediately", active))
		return false, nil
	}
	runLog().Info(fmt.Sprintf("Restart requested with %d active work unit(s); "+
		"deferring stop() until they finish (cap=%.0fs) so in-flight "+
		"turns are not amputated (#77184)", active, timeout))
	r.scaleToZeroStatus(ctx, "draining", "restart wait: status mark failed")
	deadline := pythread.Monotonic() + timeout
	lastStatusAt := 0.0
	for r.awaitableWorkCount(ctx) > 0 {
		now := pythread.Monotonic()
		if now >= deadline {
			runLog().Warn(fmt.Sprintf("Restart after-turn wait timed out after %.0fs with %d "+
				"still active; proceeding to stop()/drain which may "+
				"interrupt remaining work (#77184)", timeout, r.activeWorkCount()))
			return false, nil
		}
		if now-lastStatusAt >= 30.0 {
			runLog().Info(fmt.Sprintf("Restart deferred: waiting on %d active work unit(s) "+
				"(%d wedged and excluded, %d restart-safe and excluded; "+
				"%.0fs remaining before force drain): %s",
				r.awaitableWorkCount(ctx), r.wedgedAgentCount(ctx), r.restartSafeCronCount(ctx), deadline-now,
				pyrepr.Repr(r.describeActiveWork(ctx))))
			r.scaleToZeroStatus(ctx, "draining", "restart wait: status mark failed")
			lastStatusAt = now
		}
		if err := sleepCtx(ctx, 0.1); err != nil {
			return false, err
		}
	}
	if r.activeWorkCount() > 0 {
		runLog().Warn(fmt.Sprintf("Restart deferred wait: %d excluded work unit(s) remain "+
			"(%d wedged, %d restart-safe external cron); proceeding to stop()/drain",
			r.activeWorkCount(), r.wedgedAgentCount(ctx), r.restartSafeCronCount(ctx)))
		return false, nil
	}
	runLog().Info("Restart deferred wait complete — active work drained; proceeding to stop()")
	return true, nil
}

// Starts sd_notify heartbeats once a configured gateway is truly running
func (r *GatewayRunner) startSystemdWatchdog(ctx context.Context) bool {
	if !r.running.Load() || r.config.SystemdWatchdogSeconds <= 0 {
		return false
	}
	if r.systemdWatchdog != nil {
		return true
	}
	watchdog := gateway.NewSystemdWatchdog(true, nil)
	if !watchdog.Start(ctx) {
		return false
	}
	r.systemdWatchdog = watchdog
	watchdog.Ready("Icebath Gateway running")
	return true
}

// Stops the systemd heartbeats before a potentially long shutdown drain
func (r *GatewayRunner) stopSystemdWatchdog(ctx context.Context) {
	watchdog := r.systemdWatchdog
	if watchdog == nil {
		return
	}
	r.systemdWatchdog = nil
	watchdog.Stop(ctx)
}
