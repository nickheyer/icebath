package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	auxDefaultAuxTimeout              = 30.0
	auxCompressionTimeoutFloorSeconds = 300.0
)

// Task config of an auxiliary task, empty when it cannot be read
func auxTaskConfigOrEmpty(ctx context.Context, task string) map[string]any {
	cfg, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil || cfg == nil {
		return map[string]any{}
	}
	return cfg
}

// Configured positive no progress window of a task in seconds, 0 when unset or invalid
func auxGetTaskNoProgressTimeout(ctx context.Context, task string) float64 {
	if task == "" {
		return 0
	}
	raw, ok := auxTaskConfigOrEmpty(ctx, task)["no_progress_timeout"]
	if !ok || raw == nil {
		return 0
	}
	value, err := auFloat(raw)
	if err != nil {
		value = 0
	}
	if _, isBool := raw.(bool); isBool || value <= 0 {
		slog.Warn(fmt.Sprintf("auxiliary.%s.no_progress_timeout=%s is not a positive number of seconds; "+
			"using the built-in %.0fs default", task, pyrepr.Repr(raw), auxStreamNoProgressTimeoutSeconds))
		return 0
	}
	return value
}

// Configured request timeout of a task, the default when unset or not a number
func auxGetTaskTimeout(ctx context.Context, task string, def float64) float64 {
	if task == "" {
		return def
	}
	if raw, ok := auxTaskConfigOrEmpty(ctx, task)["timeout"]; ok && raw != nil {
		if value, err := auFloat(raw); err == nil {
			return value
		}
	}
	return def
}

// Explicit timeout first, then the task config, compression floored so long summaries are not cut off
func EffectiveAuxTimeout(ctx context.Context, task string, timeout *float64) float64 {
	if timeout != nil {
		return *timeout
	}
	effective := auxGetTaskTimeout(ctx, task, auxDefaultAuxTimeout)
	if task == "compression" {
		return max(effective, auxCompressionTimeoutFloorSeconds)
	}
	return effective
}

// Task window seam the stream watchdog tests swap
var auxGetTaskNoProgressTimeoutFn = auxGetTaskNoProgressTimeout
