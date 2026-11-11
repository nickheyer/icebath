package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
)

// Hands the kanban dispatcher the memory sample, the pressure classifier and the uncached gateway liveness ladder
func init() {
	kanban.GatewayDispatchProbes = kanban.GatewayDispatchProbeSet{
		SampleMemory:     SampleMemory,
		ClassifyPressure: ClassifyPressure,
		Liveness: func(ctx context.Context, profileDir string) (any, bool) {
			if profileDir == "" {
				profileDir = home.Dir(ctx)
			}
			l := ResolveGatewayLiveness(LivenessOptions{ProfileDir: profileDir, NoCache: true})
			return l.PID, l.ProbeError
		},
	}
}
