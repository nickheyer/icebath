package gateway

import (
	"time"
)

// Seconds a fire may spend in the handler before a still starting gateway is refused as retryable
var APIFireStartupWaitSeconds = 7.0

const apiFirePollInterval = 100 * time.Millisecond

// Runner whose drain an external supervisor started
type externalDrainRunner interface {
	ExternalDrainActive() bool
}

// Runner reporting whether it finished starting
type runningRunner interface {
	Running() bool
}

// Whether the runner refuses new work, an in process drain or one an external supervisor started
func APIRunnerIsDraining(runner any) bool {
	if drainer, ok := runner.(interface{ Draining() bool }); ok && drainer.Draining() {
		return true
	}
	external, ok := runner.(externalDrainRunner)
	return ok && external.ExternalDrainActive()
}
