package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Context carrying prepared as the executing API admission, for the runner and its cases
func BindAPIExecution(ctx context.Context, prepared *pyjson.Object) context.Context {
	return bindAPIExecution(ctx, prepared)
}
