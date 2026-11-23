package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seconds the startup restore holds the inbound gate for boot auto resume, non positive disabling the bound
func startupRestoreDrainTimeoutSecs(ctx context.Context) float64 {
	return settings.AgentGatewayStartupRestoreDrainTimeout.Get(ctx).Seconds()
}

// Seconds the boot warm up may hold the inbound gate shut, non positive disabling it
func startupWarmupTimeoutSecs(ctx context.Context) float64 {
	return settings.AgentGatewayStartupWarmupTimeout.Get(ctx).Seconds()
}

// Initializes the first turn prerequisites and returns how many tool schemas were materialized
func warmTurnMachinerySync(ctx context.Context) (int, error) {
	var enabled []string
	if cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil); err != nil {
		pkgLog().Debug("platform toolset resolution failed; warming the full tool surface", "error", err)
	} else {
		enabled = []string{}
		for name := range tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true}) {
			enabled = append(enabled, name)
		}
		sort.Strings(enabled)
	}
	defs := tools.Default.ToolDefinitions(ctx, nil, tools.DefinitionOptions{Enabled: enabled, Quiet: true})
	if settings.AgentEnvironmentProbe.Get(ctx) {
		tools.EnvironmentProbeLine(nil, false)
	}
	modelCtx := resolveGatewayModelContext(ctx, "", nil)
	pkgLog().Info(fmt.Sprintf("Model context warmed: %s -> %d tokens (%s)", modelCtx.model, modelCtx.contextLength, modelCtx.contextSource))
	return len(defs), nil
}

// Starts the boot turn machinery warm up so it overlaps the platform connects
func (r *GatewayRunner) startStartupWarmup(ctx context.Context) {
	if startupWarmupTimeoutSecs(ctx) <= 0 {
		r.startupWarmupTask = nil
		return
	}
	r.startupWarmupTask = spawnGatewayTask(ctx, "", func(ctx context.Context) (any, error) {
		r.warmTurnPrerequisites(ctx)
		return nil, nil
	})
}

// Runs a boot probe off the loop under the launch profile scope when multiplexing
func (r *GatewayRunner) runBootProbeInLaunchScope(ctx context.Context, fn func(ctx context.Context) (int, error)) (int, error) {
	if r.MultiplexProfiles() {
		ctx = gateway.ProfileRuntimeScope(ctx, home.Dir(ctx), true)
	}
	type outcome struct {
		n   int
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		n, err := fn(ctx)
		done <- outcome{n, err}
	}()
	select {
	case out := <-done:
		return out.n, out.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Initializes turn machinery before the gate opens, a failure degrading to lazy init
func (r *GatewayRunner) warmTurnPrerequisites(ctx context.Context) {
	started := time.Now()
	count, err := r.runBootProbeInLaunchScope(ctx, warmTurnMachinerySync)
	if err != nil {
		logSuppressedExc(slog.LevelWarn, err, "Turn-machinery warm-up failed; first inbound turn will initialize lazily")
		return
	}
	pkgLog().Info(fmt.Sprintf("Turn machinery warmed in %.1fs (%d tool schema(s) materialized)", time.Since(started).Seconds(), count))
}

// Bounded wait for the boot warm up, the gate opening anyway once it times out
func (r *GatewayRunner) awaitStartupWarmup(ctx context.Context) {
	task := r.startupWarmupTask
	if task == nil || task.isDone() {
		return
	}
	timeout := startupWarmupTimeoutSecs(ctx)
	if timeout <= 0 {
		return
	}
	r.waitBoundedOrRelease(ctx, []*gatewayTask{task}, timeout,
		"Turn-machinery warm-up still running after %.0fs; opening inbound gate anyway — the "+
			"first turn may see lazily initialized machinery (#99373). Warm-up continues in the background.",
		"boot turn-machinery warm-up failed after gate release", slog.LevelDebug, false)
}

// Waits for tasks up to timeout without cancelling them, logging and tracking the ones still running
func (r *GatewayRunner) waitBoundedOrRelease(ctx context.Context, tasks []*gatewayTask, timeout float64, warnFmt, lateMsg string, level slog.Level, track bool) []*gatewayTask {
	if timeout <= 0 {
		for _, task := range tasks {
			_, _ = task.wait(ctx)
		}
		return tasks
	}
	deadline := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer deadline.Stop()
waiting:
	for _, task := range tasks {
		select {
		case <-task.done:
		case <-deadline.C:
			break waiting
		case <-ctx.Done():
			break waiting
		}
	}
	var done, pending []*gatewayTask
	for _, task := range tasks {
		if task.isDone() {
			done = append(done, task)
		} else {
			pending = append(pending, task)
		}
	}
	if len(pending) > 0 {
		if strings.Count(warnFmt, "%") > 1 {
			pkgLog().Warn(fmt.Sprintf(warnFmt, timeout, len(pending)))
		} else {
			pkgLog().Warn(fmt.Sprintf(warnFmt, timeout))
		}
		late := lateFailureCallback(lateMsg, level)
		for _, task := range pending {
			task.addDoneCallback(late)
			if track {
				r.retainBackgroundTask(task)
			}
		}
	}
	return done
}

// Done callback surfacing the failure of a boot task that outlived its gate, cancellation being expected
func lateFailureCallback(message string, level slog.Level) func(*gatewayFuture) {
	return func(f *gatewayFuture) {
		if f.cancelled() {
			return
		}
		f.mu.Lock()
		err := f.err
		f.mu.Unlock()
		if err != nil {
			pkgLog().Log(context.Background(), level, message, "exc_info", err)
		}
	}
}
