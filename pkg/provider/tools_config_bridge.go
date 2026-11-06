package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.LeftCorePlatforms = func() []string {
		var out []string
		for _, f := range LeftCore() {
			if f.Platform != "" {
				out = append(out, f.Platform)
			}
		}
		return out
	}
	tools.XAIOAuthTokensReader = func(ctx context.Context, h string) error {
		env := homePoolEnv(h)
		env.Ctx = ctx
		_, err := readXAIOAuthTokens(env, true)
		return err
	}
}

// Bool like config value through the tools_config parser
func parseEnabledFlag(v any, def bool) bool { return tools.ParseEnabledFlag(v, def) }
