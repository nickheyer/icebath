package memory

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Saves values through the save_config a provider class defines itself keeping their order, false when it inherits the base one
func SaveProviderNativeConfig(ctx context.Context, p Provider, values *pyjson.Object, icebathHome string) (bool, error) {
	hosted, ok := p.(*hostedProvider)
	if !ok {
		return false, nil
	}
	overrides, err := hosted.obj.OverridesBase(ctx, "save_config")
	if err != nil || !overrides {
		return false, err
	}
	_, err = hosted.obj.Call(ctx, "save_config", []any{values, icebathHome}, nil)
	return true, err
}
