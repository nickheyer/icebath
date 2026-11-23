package gatewayrun

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Error raised from a cause, its message alone with the cause kept for unwrapping
type causedError struct {
	msg   string
	cause error
}

func (e *causedError) Error() string { return e.msg }
func (e *causedError) Unwrap() error { return e.cause }

var errNoSessionAPI = errors.New("the gateway session API is not running")

// Whether the session API serve task has ended
func sessionAPIDone(r *GatewayRunner) bool {
	select {
	case <-r.sessionAPI.task:
		return true
	default:
		return false
	}
}

// Whether this gateway began its own drain or stop, still true once the ended stop cleared draining
func ownStopBegun(r *GatewayRunner) bool {
	return r.draining.Load() || r.currentStopTask() != nil
}

// Marks the session runtime ready and starts hosted room services, publishing nothing once a stop began
func publishGatewayRuntimeReady(r *GatewayRunner) error {
	if r.sessionAPI == nil {
		return errNoSessionAPI
	}
	if r.currentStopTask() != nil {
		return nil
	}
	if sessionAPIDone(r) || !r.running.Load() || r.draining.Load() {
		return errors.New("gateway stopped before session API readiness")
	}
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
		descriptor["state"] = "ready"
	})
	return gateway.StartReadyHostedServices(r)
}

// Publishes ready and starts the systemd and idle watchdogs as one step a concurrent stop waits out or skips
func startGatewayReadyTail(ctx context.Context, r *GatewayRunner, idleExit bool) error {
	r.readyTail.Lock()
	defer r.readyTail.Unlock()
	if r.currentStopTask() != nil {
		return nil
	}
	if err := publishRuntimeReady(r); err != nil {
		return err
	}
	r.startSystemdWatchdog(ctx)
	if idleExit {
		armUnmanagedIdleExit(r)
	}
	return nil
}

// Waits out a ready publication in progress so the teardown stops everything it started
func (r *GatewayRunner) awaitReadyTail() {
	r.readyTail.Lock()
	defer r.readyTail.Unlock()
}

// Waits for shutdown, a session API listener that vanishes before any drain or stop began failing the runtime
func waitGatewayRuntime(ctx context.Context, r *GatewayRunner) error {
	if r.sessionAPI == nil {
		return errNoSessionAPI
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	shutdown := make(chan error, 1)
	go func() { shutdown <- r.WaitForShutdown(waitCtx) }()
	select {
	case err := <-shutdown:
		return err
	case <-r.sessionAPI.task:
		if !ownStopBegun(r) {
			r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
				descriptor["state"] = "failed"
			})
			return &causedError{msg: "gateway session API stopped unexpectedly", cause: r.sessionAPI.serveErr}
		}
		return <-shutdown
	}
}
