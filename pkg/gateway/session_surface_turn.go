package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Committed surface of the executing admission, nil outside a surface turn scope
func SurfaceTurn(ctx context.Context) *jsonutil.Object {
	committed, _ := ctx.Value(surfaceTurnKey{}).(*jsonutil.Object)
	return committed
}

// Whether the executing admission is a spoken voice turn on the voice chat route
func SurfaceVoiceTurn(ctx context.Context) bool { return surfaceVoiceTurn(ctx) }
