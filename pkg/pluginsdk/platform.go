package pluginsdk

import (
	"context"
	"fmt"
)

// One messaging platform, its adapter built per connection from the platform config
type Platform struct {
	Name           string
	Label          string
	RequiredEnv    []string
	InstallHint    string
	Extra          map[string]any
	Check          func(ctx context.Context) bool
	ValidateConfig func(ctx context.Context, config map[string]any) (bool, error)
	Adapter        func(ctx context.Context, config map[string]any) (*Object, error)
}

func (h *host) RegisterPlatform(platform Platform) (*Handle, error) {
	if platform.Adapter == nil {
		return nil, fmt.Errorf("platform %q has no adapter", platform.Name)
	}
	factory := h.peer.funcRef(platform.Name+".adapter", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
		var config map[string]any
		if len(args) > 0 {
			config, _ = args[0].(map[string]any)
		}
		return platform.Adapter(ctx, mapOrEmpty(config))
	})
	check := platform.Check
	if check == nil {
		check = func(context.Context) bool { return true }
	}
	kwargs := map[string]any{}
	for k, v := range platform.Extra {
		kwargs[k] = v
	}
	kwargs["name"], kwargs["label"], kwargs["adapter_factory"] = platform.Name, platform.Label, factory
	kwargs["check_fn"], kwargs["install_hint"] = h.checkRef(platform.Name+".check", check), platform.InstallHint
	if len(platform.RequiredEnv) > 0 {
		kwargs["required_env"] = platform.RequiredEnv
	}
	if platform.ValidateConfig != nil {
		kwargs["validate_config"] = h.peer.funcRef(platform.Name+".validate", func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			var config map[string]any
			if len(args) > 0 {
				config, _ = args[0].(map[string]any)
			}
			return platform.ValidateConfig(ctx, mapOrEmpty(config))
		})
	}
	return h.register("register_platform", nil, kwargs)
}
