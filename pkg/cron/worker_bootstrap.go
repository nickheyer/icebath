package cron

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Process boot hermes_bootstrap runs at import, assigned by the binary and free to relaunch the worker
var WorkerProcessBoot func()

// Runs the PM dependency boot in a worker the gateway marked, again after a relaunch
func workerBootstrap(ctx context.Context) error {
	if os.Getenv(workerMarker) == "" {
		return nil
	}
	return pm.ActivateDependencies(ctx, pm.RepoRoot())
}

// Runs the process boot before the payload is read, then drops the marker so job children never inherit it
func finishWorkerBoot() {
	if WorkerProcessBoot == nil {
		panic("cron.WorkerProcessBoot is not wired, the binary assigns it before dispatching the worker entry")
	}
	WorkerProcessBoot()
	_ = os.Unsetenv(workerMarker)
}
