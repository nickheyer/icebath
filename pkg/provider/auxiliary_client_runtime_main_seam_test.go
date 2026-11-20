package provider

import "context"

// Drops the runtime binding under the returned context
func ClearRuntimeMain(ctx context.Context) context.Context {
	return context.WithValue(ctx, auxRuntimeMainKey{}, map[string]any(nil))
}
