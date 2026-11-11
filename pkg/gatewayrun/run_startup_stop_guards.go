package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Disarms the loop liveness watchdog, floor timer and heartbeat task before shutdown loads the loop
func (r *GatewayRunner) stopLoopLivenessGuards() {
	guards := []struct {
		what string
		stop func()
	}{}
	if watchdog := r.loopLivenessWatchdog; watchdog != nil {
		guards = append(guards, struct {
			what string
			stop func()
		}{"stop gateway loop liveness watchdog", watchdog.Stop})
	}
	r.loopLivenessWatchdog = nil
	if timer := r.loopFloorTimerHandle; timer != nil {
		guards = append(guards, struct {
			what string
			stop func()
		}{"cancel gateway loop floor timer", timer.Cancel})
	}
	r.loopFloorTimerHandle = nil
	if task := r.loopHeartbeatTask; task != nil {
		guards = append(guards, struct {
			what string
			stop func()
		}{"cancel gateway loop heartbeat task", func() { task.Cancel() }})
	}
	r.loopHeartbeatTask = nil
	for _, g := range guards {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("Failed to %s", g.what), "exc_info", rec)
				}
			}()
			g.stop()
		}()
	}
}

// Pauses hosted room execution durably without interrupting accepted turns, true when it stopped in time
func (r *GatewayRunner) stopHostedRoomWorker(ctx context.Context, timeout float64) bool {
	if r.sessionAuthority != nil {
		return gateway.StopHostedService(ctx, r, timeout)
	}
	result := make(chan bool, 1)
	go func() { result <- tuigateway.StopHostedRoomService(timeout) }()
	select {
	case stopped := <-result:
		return stopped
	case <-ctx.Done():
		return false
	}
}
