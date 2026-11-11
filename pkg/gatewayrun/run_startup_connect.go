package gatewayrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Invalid profile multiplexer config the operator must fix, propagated to the startup guard instead of retried
type MultiplexConfigError struct{ msg string }

func (e *MultiplexConfigError) Error() string { return e.msg }

// Multiplexer config error carrying the operator facing message
func newMultiplexConfigError(msg string) *MultiplexConfigError {
	return &MultiplexConfigError{msg: msg}
}

// One enabled platform with its config and wired adapter, waiting for its startup connect
type pendingConnect struct {
	platform gateway.Platform
	config   *gateway.PlatformConfig
	adapter  gateway.PlatformAdapter
}

// Outcome of one startup connect, raised set when the connect escaped with an error instead
type connectResult struct {
	pendingConnect
	outcome string
	exc     error
	raised  error
}

// Connects one platform without letting it block the others
func (r *GatewayRunner) connectOneStartup(ctx context.Context, p pendingConnect) (result connectResult, err error) {
	result.pendingConnect = p
	aborted, err := r.abortStartupIfShutdownRequested(ctx, p.adapter, &p.platform)
	if err != nil {
		return result, err
	}
	if aborted {
		result.outcome = "aborted"
		return result, nil
	}
	runLog().Info(fmt.Sprintf("Connecting to %s...", p.platform))
	r.updatePlatformRuntimeStatus(ctx, string(p.platform), platformStatus{state: optStr("connecting")})
	ok, connectErr := func() (ok bool, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return r.connectInitialAdapterWithTimeout(ctx, p.adapter, p.platform)
	}()
	switch {
	case connectErr != nil:
		result.outcome, result.exc = "exception", connectErr
	case ok:
		result.outcome = "ok"
	default:
		result.outcome = "failed"
	}
	return result, nil
}

// Connects the pending adapters concurrently, nil when a restart or shutdown aborted startup mid connect
func (r *GatewayRunner) startConnectPending(ctx context.Context, pending []pendingConnect) ([]connectResult, error) {
	if len(pending) == 0 {
		return []connectResult{}, nil
	}
	tasks := make([]*gatewayTask, len(pending))
	for i, p := range pending {
		tasks[i] = spawnGatewayTask(ctx, "", func(taskCtx context.Context) (any, error) {
			return r.connectOneStartup(taskCtx, p)
		})
	}
	stillPending := func() []int {
		var out []int
		for i, t := range tasks {
			if !t.isDone() {
				out = append(out, i)
			}
		}
		return out
	}
	var open []int
	for {
		open = stillPending()
		if len(open) == 0 {
			results := make([]connectResult, len(tasks))
			for i, t := range tasks {
				value, err := t.wait(context.Background())
				if err != nil {
					results[i] = connectResult{pendingConnect: pending[i], raised: err}
					continue
				}
				results[i] = value.(connectResult)
			}
			return results, nil
		}
		if r.startupShouldAbort() {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for _, i := range open {
		tasks[i].Cancel()
	}
	for _, i := range open {
		tasks[i].wait(context.Background())
	}
	isOpen := map[int]bool{}
	for _, i := range open {
		isOpen[i] = true
	}
	teardown := append([]int{}, open...)
	for i, t := range tasks {
		if isOpen[i] || t.cancelled() {
			continue
		}
		value, err := t.wait(context.Background())
		if err == nil && value.(connectResult).outcome == "ok" {
			teardown = append(teardown, i)
		}
	}
	for _, i := range teardown {
		r.startupTeardownAdapter(ctx, pending[i].adapter, pending[i].platform)
	}
	if _, err := r.abortStartupIfShutdownRequested(ctx, nil, nil); err != nil {
		return nil, err
	}
	return nil, nil
}

// Marks a platform retrying with message and queues it for the reconnect watcher
func (r *GatewayRunner) startupQueueTransientFailure(ctx context.Context, platform gateway.Platform, adapter gateway.PlatformAdapter, platformConfig *gateway.PlatformConfig, message string, retryable *[]string) {
	r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr("retrying"), errorMessage: optStr(message)})
	*retryable = append(*retryable, fmt.Sprintf("%s: %s", platform, message))
	r.failedPlatforms.Set(platform, r.startupRetryEntry(platform, adapter, platformConfig, true))
}

// Records a fatal config startup failure with exit code 78 and requests a clean exit
func (r *GatewayRunner) startupFailFatalConfig(ctx context.Context, reason string) {
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "startup_failed", "exit_reason": reason}})
	code := gateway.GatewayFatalConfigExitCode
	r.exitCode = &code
	r.requestCleanExit(reason)
	r.startupRestoreInProgress = false
}

// Logs and degrades when a configured platform failed to start, true when startup must exit
func (r *GatewayRunner) startHandleNoConnections(ctx context.Context, connectedCount, enabledPlatformCount int, retryable, nonretryable []string) bool {
	if connectedCount != 0 {
		if len(nonretryable) > 0 {
			r.startupParkedPlatforms = true
			runLog().Error(fmt.Sprintf("%d configured platform(s) failed to start and are parked (fix the reported error, "+
				"then `icebath gateway restart`): %s. The gateway is DEGRADED — it serves the "+
				"remaining platform(s) with those unserved.", len(nonretryable), strings.Join(nonretryable, "; ")))
		}
		return false
	}
	if len(nonretryable) > 0 && len(retryable) == 0 {
		if r.startupOwnershipConflict {
			reason := strings.Join(nonretryable, "; ")
			runLog().Error(fmt.Sprintf("Gateway hit a non-retryable startup conflict: %s", reason))
			r.startupFailFatalConfig(ctx, reason)
			return true
		}
		r.startupParkedPlatforms = true
		runLog().Error(fmt.Sprintf("%d configured platform(s) failed to start and are parked (fix the reported error, then "+
			"`icebath gateway restart`): %s. The gateway is DEGRADED — it serves local clients and "+
			"cron with no messaging platform.", len(nonretryable), strings.Join(nonretryable, "; ")))
		return false
	}
	if len(nonretryable) > 0 {
		r.startupParkedPlatforms = true
		runLog().Error(fmt.Sprintf("%d platform(s) fatally misconfigured and parked: %s. "+
			"Staying alive so retryable platforms can recover.", len(nonretryable), strings.Join(nonretryable, "; ")))
	}
	if enabledPlatformCount <= 0 {
		runLog().Warn("No messaging platforms enabled.")
		runLog().Info("Gateway will continue running for cron job execution.")
		return false
	}
	if len(retryable) > 0 {
		runLog().Warn(fmt.Sprintf("Gateway started with no connected platforms — %d platform(s) queued for retry: %s",
			len(retryable), strings.Join(retryable, "; ")))
		writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: map[string]any{"gateway_state": "degraded", "exit_reason": nil}})
	}
	runLog().Warn(fmt.Sprintf("No adapter could be created for any of the %d configured platform(s). "+
		"Check that required dependencies are installed and credentials are set. "+
		"Gateway will continue for cron job execution; platforms whose plugin may still register "+
		"are queued for background retry.", enabledPlatformCount))
	return false
}
