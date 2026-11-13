package plugins

import (
	"context"
	"fmt"
	"path/filepath"
)

// Handler of one gateway hook directory, called with the event type and its context
type GatewayHookHandler func(ctx context.Context, eventType string, hookContext map[string]any) (any, error)

// Loads the handler the guest a gateway hook directory declares in HOOK.yaml registers, or why the directory was skipped
func LoadGatewayHook(ctx context.Context, hookName, hookDir string) (GatewayHookHandler, string, error) {
	mf := guestManifestFile(filepath.Join(hookDir, "HOOK.yaml"), hookDir)
	if mf == nil {
		return nil, "HOOK.yaml declares no runtime: wasm or runtime: program entry", nil
	}
	inst, err := GetPluginManager(ctx).guestProviderLoad(mf, "gateway_hook", nil, false, nil)
	if err != nil {
		return nil, "", err
	}
	first, err := inst.callable(ctx)
	if err != nil {
		return nil, "", err
	}
	if first == nil {
		return nil, fmt.Sprintf("hook %s registers no handler with RegisterGatewayHook", hookName), nil
	}
	return func(ctx context.Context, eventType string, hookContext map[string]any) (any, error) {
		fn, err := inst.callable(ctx)
		if err != nil {
			return nil, err
		}
		if fn == nil {
			return nil, fmt.Errorf("hook %s no longer registers a handler", hookName)
		}
		return fn.Call(ctx, []any{eventType, hookContext}, nil)
	}, "", nil
}
