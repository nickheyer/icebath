package app

import (
	"context"
	"time"
)

// Seconds a finished process stays on the dock before it leaves
const processDockRetainSeconds = 60

// Dock status of a finished process by its completion reason
var processStatusByReason = map[string]string{"killed": "killed", "lost": "lost", "failed_start": "failed"}

// Glyph of each process dock status
var processDockGlyph = map[string]string{"running": "⚙", "done": "✔", "failed": "✘", "killed": "✘", "lost": "?"}

// Catalog key of the exit verdict of each finished status
var processVerdictKeys = map[string]string{"done": "cli.dock.exit_code", "failed": "cli.dock.exit_code",
	"killed": "cli.dock.verdict_killed", "lost": "cli.dock.verdict_lost"}

// Kills a dock process through the registry and returns its result
func killDockProcess(ctx context.Context, registry cliProcessRegistry, processID string) map[string]any {
	return registry.killProcess(ctx, processID)
}

// Wall clock seconds as a float
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }
