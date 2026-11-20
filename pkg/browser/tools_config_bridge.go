package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the browser runtime and provider registry facts the tools picker reads
func init() {
	hooks := &tools.ToolsConfigHooks
	hooks.BrowserRegistry = &tools.PickerRegistry{
		List: func(ctx context.Context, env *tools.Env) []tools.PickerProvider {
			var out []tools.PickerProvider
			for _, p := range BrowserProviderRegistry.ListProviders(ctx, cfgOf(env).homeDir()) {
				out = append(out, p)
			}
			return out
		},
		Get: func(ctx context.Context, env *tools.Env, name string) tools.PickerProvider {
			if p := BrowserProviderRegistry.GetProvider(ctx, name, cfgOf(env).homeDir()); p != nil {
				return p
			}
			return nil
		},
	}
	hooks.LightpandaBinary = findLightpandaBinary
	hooks.LightpandaInstallHint = func() string { return lightpandaInstallHint }
	hooks.ChromiumExecutable = func(env *tools.Env) string { return chromiumExecutable(env, true) }
	hooks.ChromiumMissingHint = chromiumMissingHint
	hooks.BakedChromiumFile = func() string { return bakedExecutableFile }
}
