package selfupdate

import "context"

// One human line of an active work entry, the drain report wording the gateway CLI prints
func DescribeActiveWorkUnit(ctx context.Context, unit any, h string) (string, error) {
	return describeActiveWorkUnit(ctx, unit, h)
}
