package gatewayrun

import (
	"context"
	"sync"
)

// Running gateway API listener, its exit request and the task serving it until shutdown finished
type gatewayAPIHandle struct {
	exitOnce   sync.Once
	shouldExit chan struct{}
	task       chan struct{}
	// Error the serve task ended with, set before task closes and nil for a clean exit
	serveErr error
}

// Asks the listener to drain its sockets and waits for its serve task, never stopping the authority
func stopGatewayAPI(ctx context.Context, handle *gatewayAPIHandle) {
	handle.exitOnce.Do(func() { close(handle.shouldExit) })
	select {
	case <-handle.task:
	case <-ctx.Done():
	}
}
