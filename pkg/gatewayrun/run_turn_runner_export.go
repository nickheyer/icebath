package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Chat text, failure reason and retryable flag a messaging turn answers a resolution failure with
func ResolutionFailureReply(ctx context.Context, err error, platform gateway.Platform) (string, string, bool) {
	return resolutionFailureReply(ctx, err, platform)
}
