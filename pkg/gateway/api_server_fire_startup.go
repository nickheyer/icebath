package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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

// Nil once the runner finished starting or without a runner, else the retryable 503 after the startup budget
func APIRefuseUntilStarted(ctx context.Context, runner any, jobID string, receivedAt time.Time) (*WebResponse, error) {
	if runner == nil {
		return nil, nil
	}
	deadline := receivedAt.Add(time.Duration(APIFireStartupWaitSeconds * float64(time.Second)))
	for {
		draining := APIRunnerIsDraining(runner)
		running := true
		if holder, ok := runner.(runningRunner); ok {
			running = holder.Running()
		}
		if !draining && running {
			return nil, nil
		}
		if draining || !time.Now().Before(deadline) {
			return WebJSONResponse(pyjson.ObjectOf("error", "gateway unreachable; retry", "job_id", jobID),
				http.StatusServiceUnavailable, map[string]string{"Retry-After": "60"})
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(apiFirePollInterval):
		}
	}
}

// Startup refusal of a fire, else the live adapters of the gateway runner, nil adapters without a runner
func APILiveAdaptersOnceStarted(ctx context.Context, adapterRunner, appRunner any, jobID string, receivedAt time.Time) (*WebResponse, *pydict.Dict[Platform, PlatformAdapter], error) {
	runner := adapterRunner
	if runner == nil {
		runner = appRunner
	}
	if runner == nil {
		if current := CurrentGatewayRunner(); current != nil {
			runner = current
		}
	}
	refusal, err := APIRefuseUntilStarted(ctx, runner, jobID, receivedAt)
	if err != nil || refusal != nil {
		return refusal, nil, err
	}
	holder, ok := runner.(launchAdaptersRunner)
	if !ok || holder == nil {
		return nil, nil, nil
	}
	adapters := holder.Adapters()
	if adapters == nil || adapters.Len() == 0 {
		return nil, nil, nil
	}
	return nil, adapters, nil
}
