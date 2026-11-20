package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Configured CDP override with no network I/O, the live connect then BROWSER_CDP_URL then browser.cdp_url
func CDPOverrideRaw(env *tools.Env) string { return cfgOf(env).cdpOverrideRaw() }

// Closes one profile home's sessions, supervisors and harness daemons and drops its cached config
func CleanupProfileBrowsers(ctx context.Context, env *tools.Env, homeKey string) error {
	return cleanupProfileBrowsers(ctx, env, homeKey)
}
