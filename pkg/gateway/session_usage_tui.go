package gateway

import (
	"math"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Float of a numeric stats figure, false for a missing or non numeric one
func usageFloat(value any) (float64, bool) {
	if value == nil {
		return 0, false
	}
	if _, isText := value.(string); isText {
		return 0, false
	}
	f, err := pyint.Float(value)
	return f, err == nil
}

// Python int or float figure, bools included as ints are
func usageIsNumber(value any) bool {
	switch value.(type) {
	case bool, float64, float32:
		return true
	}
	_, ok := pyrepr.Int(value)
	return ok
}

// Python int figure, bools included as ints are
func usageIsInt(value any) bool {
	if _, ok := value.(bool); ok {
		return true
	}
	return pyIsInt(value)
}

// Whole percent of part over whole, half to even and clamped to 0 through 100
func usagePercent(part, whole float64) int64 {
	return int64(math.Max(0, math.Min(100, math.RoundToEven(part/whole*100))))
}

// Stats in the TUI usage shape its status bar and usage command read, absent figures omitted
func tuiUsage(stats *pyjson.Object) *pyjson.Object {
	get := func(key string) any {
		value, _ := stats.Get(key)
		return value
	}
	usage := pyjson.NewObject()
	usage.Set("model", pyrepr.Or(get("model"), ""))
	usage.Set("input", pyrepr.Or(pyrepr.Or(get("input_tokens"), get("prompt_tokens")), int64(0)))
	usage.Set("output", pyrepr.Or(pyrepr.Or(get("output_tokens"), get("completion_tokens")), int64(0)))
	usage.Set("reasoning", pyrepr.Or(get("reasoning_tokens"), int64(0)))
	usage.Set("prompt", pyrepr.Or(get("prompt_tokens"), int64(0)))
	usage.Set("completion", pyrepr.Or(get("completion_tokens"), int64(0)))
	usage.Set("total", pyrepr.Or(get("total_tokens"), int64(0)))
	usage.Set("calls", pyrepr.Or(get("api_calls"), int64(0)))
	used, maximum := pyrepr.Or(get("last_prompt_tokens"), int64(0)), pyrepr.Or(get("context_length"), int64(0))
	if pyTruthy(used) && pyTruthy(maximum) {
		usedF, _ := usageFloat(used)
		maxF, _ := usageFloat(maximum)
		if maxF < usedF {
			used, usedF = maximum, maxF
		}
		source := pyrepr.Or(get("context_source"), "provider_usage")
		usage.Set("context_used", used)
		usage.Set("context_max", maximum)
		usage.Set("context_percent", usagePercent(usedF, maxF))
		usage.Set("context_source", source)
		usage.Set("context_estimated", !pyrepr.Equal(source, "provider_usage"))
	}
	if get("context_source") != nil {
		usage.Set("compressions", pyrepr.Or(get("compression_count"), int64(0)))
	}
	promptTotal, _ := usageFloat(pyrepr.Or(get("prompt_tokens"), int64(0)))
	cacheRead, _ := usageFloat(pyrepr.Or(get("cache_read_tokens"), int64(0)))
	if promptTotal > 0 && cacheRead > 0 {
		usage.Set("cache_hit_pct", usagePercent(cacheRead, promptTotal))
	}
	for _, pair := range [][2]string{{"avg_latency_s", "avg_latency_s"}, {"avg_tps", "tokens_per_s"}} {
		if value := get(pair[1]); usageIsNumber(value) {
			f, _ := usageFloat(value)
			usage.Set(pair[0], pyint.Round(f, 1))
		}
	}
	if value := get("active_subagents"); usageIsInt(value) {
		usage.Set("active_subagents", value)
	}
	switch gauge := get("plan_gauge").(type) {
	case *pyjson.Object, map[string]any:
		usage.Set("plan_gauge", gauge)
	}
	return usage
}
