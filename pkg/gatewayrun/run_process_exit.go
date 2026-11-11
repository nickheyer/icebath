package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Polls for the planned stop marker naming this process and drives the shutdown handler on the loop once
func runPlannedStopWatcher(stop *pythread.Event, r *GatewayRunner, loop gateway.EventLoop, shutdownHandler func(sig *int), pollInterval float64) {
	markerPath := gateway.PlannedStopMarkerPath()
	for !stop.IsSet() {
		fired := func() (fired bool) {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Planned-stop watcher tick error: %v", rec))
					fired = false
				}
			}()
			if _, err := os.Stat(markerPath); err != nil || r.draining.Load() || !r.running.Load() {
				return false
			}
			targetsSelf, err := gateway.PlannedStopMarkerTargetsSelf()
			if err != nil {
				runLog().Debug(fmt.Sprintf("Planned-stop watcher tick error: %v", err))
				return false
			}
			if !targetsSelf {
				return false
			}
			if err := loop.CallSoonThreadsafe(func() { shutdownHandler(nil) }); err != nil {
				runLog().Debug(fmt.Sprintf("Planned-stop watcher tick error: %v", err))
				return false
			}
			return true
		}()
		if fired {
			return
		}
		stop.Wait(pollInterval)
	}
}

// Refuses to start on an unparseable config.yaml, printing the error and exiting 2
func guardCorruptUserConfig(ctx context.Context) error {
	err := config.RequireParseableUserConfig(ctx, home.Dir(ctx), false)
	var invalid *config.InvalidUserConfigError
	if errors.As(err, &invalid) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", invalid.Error())
		return &gateway.SystemExitError{Code: 2}
	}
	return err
}

// Releases the pid file and lock, stamps the clean exit, flushes metrics and logs, then hard exits
func exitAfterGracefulShutdown(ctx context.Context, exitCode int) {
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		_ = stream.Sync()
	}
	steps := []func() (struct{}, error){
		func() (struct{}, error) {
			gateway.RemovePidFile()
			gateway.ReleaseGatewayRuntimeLock()
			return struct{}{}, nil
		},
		func() (struct{}, error) {
			observability.StampExit("clean", "")
			code := exitCode
			gateway.MarkExited(ctx, &code, "graceful_shutdown", "")
			return struct{}{}, nil
		},
		func() (struct{}, error) {
			observability.FlushBeforeHardExit(ctx, observability.ExitFlushTimeout)
			return struct{}{}, nil
		},
		func() (struct{}, error) {
			logging.DrainLogQueue(time.Second)
			return struct{}{}, nil
		},
	}
	for _, step := range steps {
		bestEffort(step, "")
	}
	os.Exit(exitCode)
}
