package agent

import (
	"context"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	DefaultContextTimeoutSeconds      = 120.0
	DefaultContextTotalCeilingSeconds = 600.0
)

// CompressionDeadlineFence is the commit fence surface the deadline scope reads live and pushes back
type CompressionDeadlineFence interface {
	DeadlineMonotonic() (float64, bool)
	ExtendDeadline(seconds float64)
}

// Loads the compression section of the config, an empty one when the config or section is unusable
var loadCompressionConfig = func(ctx context.Context) any {
	raw, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return map[string]any{}
	}
	switch section := raw["compression"].(type) {
	case map[string]any:
		return section
	case config.Tree:
		return map[string]any(section)
	}
	return map[string]any{}
}

// Idle timeout and total ceiling seconds of context compression, nil config read from the config file
func ResolveContextCompressionTimeouts(ctx context.Context, compressionCfg any) (float64, float64) {
	idle, ceiling := DefaultContextTimeoutSeconds, DefaultContextTotalCeilingSeconds
	cfg := compressionCfg
	if cfg == nil {
		cfg = loadCompressionConfig(ctx)
	}
	if tree, ok := cfg.(config.Tree); ok {
		cfg = map[string]any(tree)
	}
	if m, ok := cfg.(map[string]any); ok {
		if v := m["context_timeout_seconds"]; v != nil {
			if f, err := pyint.Float(v); err == nil {
				idle = f
			}
		}
		if v := m["context_total_ceiling_seconds"]; v != nil {
			if f, err := pyint.Float(v); err == nil && f > 0 {
				ceiling = f
			}
		}
	}
	if idle > 0 {
		ceiling = max(ceiling, idle)
		if auxBudget := provider.EffectiveAuxTimeout(ctx, "compression", nil); auxBudget > 0 {
			ceiling = max(ceiling, auxBudget)
			idle = max(idle, min(auxBudget, ceiling))
		}
	}
	return idle, ceiling
}

// Total limit for a host that started waiting at waitStarted, longer once prompt reading pushed the deadline back
func EffectiveCeiling(fence CompressionDeadlineFence, ceiling, waitStarted float64) float64 {
	deadline, ok := fence.DeadlineMonotonic()
	if !ok {
		return ceiling
	}
	return max(ceiling, deadline-waitStarted)
}

// Shares the fence deadline live with the summary stream and pushes it back while the server reads the prompt
func CompressionDeadlineScope(ctx context.Context, fence CompressionDeadlineFence) context.Context {
	if fence == nil {
		return ctx
	}
	ctx = provider.AuxStreamDeadline(ctx, func() (time.Time, bool) {
		deadline, ok := fence.DeadlineMonotonic()
		if !ok {
			return time.Time{}, false
		}
		return time.Now().Add(time.Duration((deadline - pythread.Monotonic()) * float64(time.Second))), true
	})
	return provider.PromptReadingHook(ctx, fence.ExtendDeadline)
}
