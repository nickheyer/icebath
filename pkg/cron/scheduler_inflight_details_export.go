package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Per in flight run its job id, elapsed seconds, external worker pid and restart safety
func GetRunningJobDetails() []RunningJobDetail { return getRunningJobDetails() }

// Job ids of the home in ctx in flight longer than their stale allowance
func GetWedgedJobIDs(ctx context.Context) map[string]bool {
	return openCronStore(home.Dir(ctx), time.Now).getWedgedJobIDs(ctx)
}
