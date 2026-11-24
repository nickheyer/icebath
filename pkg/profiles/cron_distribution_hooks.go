package profiles

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Cron store access pkg/cron installs in init for the distribution installer, nil in a binary that does not link it
var CronDistributionHooks struct {
	LoadJobs             func(ctx context.Context, home string) ([]any, error)
	ImportJobDefinitions func(ctx context.Context, home string, shipped *jsonutil.Object, pausedReason string) error
}
