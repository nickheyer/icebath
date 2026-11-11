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

var errNoSessionAPI = errors.New("'NoneType' object has no attribute 'task'")

// Whether the session API serve task has ended
func sessionAPIDone(r *GatewayRunner) bool {
	select {
	case <-r.sessionAPI.task:
		return true
	default:
		return false
	}
}

// Marks the session runtime ready with its capabilities and starts the ready hosted room services
func publishGatewayRuntimeReady(r *GatewayRunner) error {
	if r.sessionAPI == nil {
		return errNoSessionAPI
	}
	if sessionAPIDone(r) || !r.running.Load() || r.draining.Load() {
		return errors.New("gateway stopped before session API readiness")
	}
	r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
		descriptor["state"] = "ready"
		descriptor["capabilities"] = []any{"session-authority-v1", "durable-admission-v1", "event-replay-v1"}
	})
	return gateway.StartReadyHostedServices(r)
}

// Waits for shutdown, a session API listener that vanishes outside a drain failing the runtime
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
		if !r.draining.Load() {
			r.sessionRuntimeDescriptor.update(func(descriptor map[string]any) {
				descriptor["state"] = "failed"
				descriptor["capabilities"] = []any{}
			})
			return &causedError{msg: "gateway session API stopped unexpectedly", cause: r.sessionAPI.serveErr}
		}
		return <-shutdown
	}
}
