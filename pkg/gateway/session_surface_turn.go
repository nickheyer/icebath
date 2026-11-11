package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Committed surface of the executing admission, nil outside a surface turn scope
func SurfaceTurn(ctx context.Context) *pyjson.Object {
	committed, _ := ctx.Value(surfaceTurnKey{}).(*pyjson.Object)
	return committed
}

// Whether the executing admission is a spoken voice turn on the voice chat route
func SurfaceVoiceTurn(ctx context.Context) bool { return surfaceVoiceTurn(ctx) }
