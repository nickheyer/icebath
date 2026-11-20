package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Runtime status publication whose failure never aborts or blocks the caller
func writeRuntimeStatusQuiet(ctx context.Context, update gateway.RuntimeStatusUpdate) {
	defer func() { _ = recover() }()
	_, _ = gateway.PublishRuntimeStatus(ctx, update)
}
