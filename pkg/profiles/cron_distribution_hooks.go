package profiles

import (
	"context"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Cron store access pkg/cron installs in init for the distribution installer, nil in a binary that does not link it
var CronDistributionHooks struct {
	LoadJobs             func(ctx context.Context, home string) ([]any, error)
	ImportJobDefinitions func(ctx context.Context, home string, shipped *pyjson.Object, pausedReason string) error
}
