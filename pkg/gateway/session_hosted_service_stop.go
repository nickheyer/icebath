package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Hosted room service that stops its worker within a timeout without interrupting accepted turns
type hostedServiceStopper interface {
	Stop(timeout float64) bool
}

// Stops the hosted room service of every authority within one shared deadline, true when all stopped
func StopHostedService(ctx context.Context, runner any, timeout float64) bool {
	deadline := pythread.Monotonic() + max(0, timeout)
	stopped := true
	for _, a := range allAuthorities(runner) {
		service, ok := a.HostedRoomService.(hostedServiceStopper)
		if !ok || service == nil {
			continue
		}
		result := make(chan bool, 1)
		budget := max(0, deadline-pythread.Monotonic())
		go func() { result <- service.Stop(budget) }()
		select {
		case ok := <-result:
			stopped = ok && stopped
		case <-ctx.Done():
			return false
		}
	}
	return stopped
}
