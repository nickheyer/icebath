package mcp

import (
	"context"
	"log/slog"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Reports configured MCP servers or enabled plugin MCP packages, true when the config cannot be read
func HasConfiguredServers(ctx context.Context) bool { return hasConfiguredServers(ctx) }

// Forgets every started, running and armed discovery after waiting a second for each run
func ResetDiscoveryStateForTests() {
	discoveryMu.Lock()
	runs := make([]*discoveryRun, 0, len(discoveryRuns))
	for _, r := range discoveryRuns {
		runs = append(runs, r)
	}
	deferred := discoveryDeferred
	discoveryStarted = map[string]bool{}
	discoveryRuns = map[string]*discoveryRun{}
	discoveryDeferred = nil
	discoveryMu.Unlock()
	if deferred != nil && deferred.timer != nil {
		deferred.timer.Stop()
	}
	for _, r := range runs {
		select {
		case <-r.done:
		case <-time.After(time.Second):
		}
	}
}

// Makes fn the background start a deferred discovery fires until the returned restore runs
func ReplaceBackgroundDiscoveryStartForTests(fn func(ctx context.Context, env *tools.Env, logger *slog.Logger, threadName string)) (restore func()) {
	previous := startBackgroundDiscovery
	startBackgroundDiscovery = fn
	return func() { startBackgroundDiscovery = previous }
}

// Whether a deferred discovery start is armed and whether a timer will fire it
func DeferredDiscoveryForTests() (armed, clocked bool) {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	return discoveryDeferred != nil, discoveryDeferred != nil && discoveryDeferred.timer != nil
}

// Stops the timer of an armed deferred discovery start, the start staying armed
func CancelDeferredDiscoveryTimerForTests() {
	discoveryMu.Lock()
	defer discoveryMu.Unlock()
	if discoveryDeferred != nil && discoveryDeferred.timer != nil {
		discoveryDeferred.timer.Stop()
	}
}
