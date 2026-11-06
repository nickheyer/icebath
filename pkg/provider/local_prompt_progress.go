package provider

import (
	"context"
	"math"
	"time"
)

type promptReadingHookKey struct{}

// Copy of chat completions kwargs asking llama server for prompt progress chunks through extra_body
func WithPromptProgressKwargs(kwargs map[string]any) map[string]any {
	merged := map[string]any{}
	if extra, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			merged[k] = v
		}
	}
	if _, ok := merged["return_progress"]; !ok {
		merged["return_progress"] = true
	}
	out := make(map[string]any, len(kwargs)+1)
	for k, v := range kwargs {
		out[k] = v
	}
	out["extra_body"] = merged
	return out
}

// Integer count of a progress field, false when missing or not an int
func promptProgressCount(progress map[string]any, key string) (int, bool) {
	raw, ok := progress[key]
	if !ok {
		return 0, false
	}
	n, err := auxPyIntCall(raw)
	return n, err == nil
}

// Context whose work hears each stretch of prompt reading through the hook, nil uninstalling it
func PromptReadingHook(ctx context.Context, hook func(seconds float64)) context.Context {
	return context.WithValue(ctx, promptReadingHookKey{}, hook)
}

// Prompt reading hook installed on the context, nil when none
func CurrentPromptReadingHook(ctx context.Context) func(seconds float64) {
	if ctx == nil {
		return nil
	}
	hook, _ := ctx.Value(promptReadingHookKey{}).(func(seconds float64))
	return hook
}

// Monotonic clock seam the reading clock measures wall time with
var promptReadingNow = time.Now

// Time llama server spends reading the prompt, counted only for chunks reporting new tokens read
type PromptReadingClock struct {
	mark      time.Time
	processed int
	serverMS  int
	Seconds   float64
}

// Reading clock whose first stretch starts at the given instant
func NewPromptReadingClock(started time.Time) *PromptReadingClock {
	return &PromptReadingClock{mark: started, processed: -1}
}

// Counts reading up to this progress chunk and tells the context hook, false when nothing new was read
func (c *PromptReadingClock) Tick(ctx context.Context, progress map[string]any) bool {
	now := promptReadingNow()
	wall := now.Sub(c.mark).Seconds()
	c.mark = now
	processed, okProcessed := promptProgressCount(progress, "processed")
	serverMS, okServer := promptProgressCount(progress, "time_ms")
	if !okProcessed || processed <= c.processed {
		return false
	}
	server := wall
	if okServer {
		server = float64(serverMS-c.serverMS) / 1000
	}
	c.processed = processed
	if okServer {
		c.serverMS = serverMS
	}
	spent := math.Max(0, math.Min(wall, server))
	c.Seconds += spent
	if hook := CurrentPromptReadingHook(ctx); hook != nil {
		hook(spent)
	}
	return true
}
