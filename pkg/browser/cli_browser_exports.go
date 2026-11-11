package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Closes every live browser session so the next tool call picks up the current backend
func CleanupAllBrowsers(ctx context.Context, env *tools.Env) {
	cleanupAllBrowsers(tools.WithEnvHome(ctx, env), env)
}

// Starts the CDP supervisor of a task so dialogs and the frame tree reach the next snapshot
func EnsureCDPSupervisor(env *tools.Env, taskID string) { ensureCDPSupervisor(env, taskID) }

// Stops the CDP supervisor of a task, logging any failure
func StopCDPSupervisor(env *tools.Env, taskID string) { stopCDPSupervisor(env, taskID) }

// Configured cloud browser provider, nil for local mode
func GetCloudProvider(env *tools.Env) (BrowserProvider, error) { return getCloudProvider(env) }

// Local browser engine from browser.engine, auto when unset
func GetBrowserEngine(env *tools.Env) string { return cfgOf(env).engine() }
