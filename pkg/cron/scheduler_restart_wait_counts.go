package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// In flight cron runs split into awaitable, wedged and restart safe for the gateway restart wait
type RestartWaitCronCounts struct {
	Awaitable   int
	Wedged      int
	RestartSafe int
}

// Disjoint per run split of the in flight cron runs the restart wait holds for, skips and leaves running
func GetRestartWaitCronCounts(ctx context.Context) RestartWaitCronCounts {
	wedged := openCronStore(home.Dir(ctx), time.Now).wedgedInflightKeys(ctx)
	inflight.Lock()
	running := map[inflightKey]bool{}
	for k, live := range inflight.running {
		if live {
			running[k] = true
		}
	}
	for k := range inflight.fireOwners {
		running[k] = true
	}
	restartSafe := map[inflightKey]bool{}
	for k, isolated := range inflight.scopeIsolated {
		if isolated && running[k] && !wedged[k] {
			restartSafe[k] = true
		}
	}
	inflight.Unlock()
	counts := RestartWaitCronCounts{RestartSafe: len(restartSafe)}
	for k := range running {
		switch {
		case wedged[k]:
			counts.Wedged++
		case !restartSafe[k]:
			counts.Awaitable++
		}
	}
	return counts
}

// Marks every in flight run interrupted with the reason and returns the job ids it marked
func MarkRunningJobsInterrupted(ctx context.Context, reason string) []string {
	return markRunningJobsInterrupted(ctx, reason, nil)
}
