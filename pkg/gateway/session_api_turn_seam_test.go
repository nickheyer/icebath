package gateway

import (
	"context"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Context carrying prepared as the executing API admission, for the runner and its cases
func BindAPIExecution(ctx context.Context, prepared *jsonutil.Object) context.Context {
	return bindAPIExecution(ctx, prepared)
}
