package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Agent side usage and context reads of the session readers, assigned from pkg/agent in cmd/icebath/wiring.go
var ContextAgent struct {
	UsageStats      func(ctx context.Context, agent any) *pyjson.Object
	Breakdown       func(ctx context.Context, agent any, conversation []map[string]any) (*pyjson.Object, error)
	StoredBreakdown func(ctx context.Context, systemPrompt string, tools []map[string]any, conversation []map[string]any) (*pyjson.Object, error)
	Marks           func(agent any) any
}

// Row counters a stored session usage reads
var storedUsageCounters = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"}

// Python int of a row figure, zero for a falsy one
func usageRowInt(row *pyjson.Object, key string) (int64, error) {
	value, _ := row.Get(key)
	if !pyTruthy(value) {
		return 0, nil
	}
	return pyint.Int64(value)
}

// Cumulative counters of a session row in the usage stats shape with cache reads and writes in the prompt total
func storedStats(row *pyjson.Object) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	counts := map[string]int64{}
	for _, key := range storedUsageCounters {
		n, err := usageRowInt(row, key)
		if err != nil {
			return nil, err
		}
		counts[key] = n
		out.Set(key, n)
	}
	calls, err := usageRowInt(row, "api_call_count")
	if err != nil {
		return nil, err
	}
	model, _ := row.Get("model")
	if !pyTruthy(model) {
		model = ""
	}
	out.Set("prompt_tokens", counts["input_tokens"]+counts["cache_read_tokens"]+counts["cache_write_tokens"])
	out.Set("completion_tokens", counts["output_tokens"])
	out.Set("total_tokens", counts["input_tokens"]+counts["output_tokens"])
	out.Set("api_calls", calls)
	out.Set("model", model)
	return out, nil
}

// Status bar usage of a resume or info snapshot, from the live agent once it called the model
func snapshotUsage(ctx context.Context, a *SessionAuthority, ref SessionRef, agent any) (*pyjson.Object, error) {
	if !nilAgent(agent) {
		result := ContextAgent.UsageStats(ctx, agent)
		if calls, _ := result.Get("api_calls"); pyTruthy(calls) {
			live, _ := a.live(ref.SessionID)
			result.Set("compression_count", carriedCompressionCount(live, agent))
			return tuiUsage(result), nil
		}
	}
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil || row == nil {
		return nil, err
	}
	stored, err := storedStats(row)
	if err != nil {
		return nil, err
	}
	return tuiUsage(stored), nil
}

// Session stats verb, the same dict a session usage frame carries
func stats(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile") {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := c.authority.Resolve(ctx, c.actor, ref); err != nil {
		return nil, err
	}
	agent, err := c.authority.agent(ref)
	if err != nil {
		return nil, err
	}
	result := ContextAgent.UsageStats(ctx, agent)
	live, _ := c.authority.live(ref.SessionID)
	result.Set("compression_count", carriedCompressionCount(live, agent))
	return result, nil
}
