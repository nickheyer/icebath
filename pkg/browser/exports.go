package browser

import "github.com/nickheyer/icebath/pkg/tools"

// Chromium binary the launcher would run, the configured override honoured when allowed
func ChromiumExecutable(env *tools.Env, allowOverride bool) string {
	return chromiumExecutable(env, allowOverride)
}

// Launches or attaches the Chromium for key, returning it or the launch error text
func LaunchChromium(env *tools.Env, key string, opts LaunchOptions) (*LaunchedBrowser, string) {
	return launchChromium(env, key, opts)
}

// Stops the Chromium this process launched or leased under key
func StopBrowser(key string) { stopBrowser(key) }
