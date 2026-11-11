package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Multiplex migration preflight as the JSON the CLI plan prints, for the dashboard plan route
func MigrationPlanDict(ctx context.Context) (*pyjson.Object, error) {
	plan, err := BuildMigrationPlan(ctx)
	if err != nil {
		return nil, err
	}
	return plan.ToDict(), nil
}
