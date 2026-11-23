package agent

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
)

// CompressionDeadlineFence is the commit fence surface the deadline scope reads live and pushes back
type CompressionDeadlineFence interface {
	DeadlineMonotonic() (float64, bool)
	ExtendDeadline(seconds float64)
}

// Effective aux compression request budget the idle window is floored at, swapped by tests
var compressionAuxTimeout = provider.EffectiveAuxTimeout

// Idle timeout and total ceiling seconds of context compression, a ceiling at or below zero taking the default
func ResolveContextCompressionTimeouts(ctx context.Context) (float64, float64) {
	idle := settings.CompressionContextTimeoutSeconds.Get(ctx).Seconds()
	ceiling := settings.CompressionContextTotalCeilingSeconds.Get(ctx).Seconds()
	if ceiling <= 0 {
		ceiling = settings.CompressionContextTotalCeilingSeconds.Default.Seconds()
	}
	if idle > 0 {
		ceiling = max(ceiling, idle)
		if auxBudget := compressionAuxTimeout(ctx, "compression", nil); auxBudget > 0 {
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
