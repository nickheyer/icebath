package tuiserver

import (
	"sync"
)

// Readiness budgets, the overlapping poll join grace and the retryable in progress code
const (
	readinessShareWaitSeconds       = 4.0
	readinessJoinGraceSeconds       = 0.5
	readinessStatusShareWaitSeconds = 12.0
	readinessInProgressErr          = 5097
)

// One single flighted readiness probe, its answer or failure set once it settles
type readinessFuture struct {
	done      chan struct{}
	result    map[string]any
	failure   any
	cancelled bool
}

// Dedicated readiness executor and the probes in flight by kind and profile
var (
	readinessPool     = newRPCWorkerPool(max(2, min(4, envInt("ICEBATH_TUI_RPC_POOL_WORKERS", 8))))
	readinessMu       sync.Mutex
	readinessInflight = map[[2]string]*readinessFuture{}
)

var configSetupRegistry = &handlerRegistry{}

func init() {
	registerAtExit(func() { readinessPool.Shutdown(true) })
	configSetupRegistry.install()
}
