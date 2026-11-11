package agent

import (
	"context"
	"log/slog"
	"math/big"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// OneshotAuxCounters are the counters summed per auxiliary task into a one shot usage ledger
var OneshotAuxCounters = []string{"api_calls", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
	"reasoning_tokens", "estimated_cost_usd"}

// OneshotAuxiliaryUsage is the per task auxiliary usage of the session lineage, empty without a store or session
func OneshotAuxiliaryUsage(ctx context.Context, db *state.SessionDB, sessionID string) *pyjson.Object {
	if db == nil || sessionID == "" {
		return pyjson.NewObject()
	}
	usage, err := db.AuxiliaryUsageByTask(ctx, sessionID)
	if err != nil {
		slog.Debug("oneshot: auxiliary usage read failed", "error", err)
		return pyjson.NewObject()
	}
	return usage
}

// AttachAuxiliaryUsage stores the run's auxiliary usage on result as the delta against the pre turn snapshot
func AttachAuxiliaryUsage(ctx context.Context, result map[string]any, db *state.SessionDB, before *pyjson.Object, fallbackSessionID string) {
	waitForTitleUpgrades(10 * time.Second)
	sessionID, _ := result["session_id"].(string)
	if !pyrepr.Truthy(result["session_id"]) {
		sessionID = fallbackSessionID
	}
	after := OneshotAuxiliaryUsage(ctx, db, sessionID)
	byTask := pyjson.NewObject()
	for _, task := range after.Keys {
		counters, _ := pyjson.AsObject(after.Vals[task])
		var prior *pyjson.Object
		if before != nil {
			prior, _ = pyjson.AsObject(before.GetOr(task, nil))
		}
		delta := pyjson.NewObject()
		changed := false
		for _, key := range OneshotAuxCounters {
			d := auxNumSub(auxCounter(counters, key), auxCounter(prior, key))
			delta.Set(key, d)
			changed = changed || pyrepr.Truthy(d)
		}
		if changed {
			byTask.Set(task, delta)
		}
	}
	result["auxiliary_usage"] = byTask
}

// Counter value when truthy, the integer zero otherwise
func auxCounter(counters *pyjson.Object, key string) any {
	if counters == nil {
		return int64(0)
	}
	v := counters.GetOr(key, nil)
	if !pyrepr.Truthy(v) {
		return int64(0)
	}
	return v
}

// Python subtraction of two counters, integers staying exact and a float operand making a float
func auxNumSub(a, b any) any {
	x, okA := auxBigInt(a)
	y, okB := auxBigInt(b)
	if okA && okB {
		diff := new(big.Int).Sub(x, y)
		if diff.IsInt64() {
			return diff.Int64()
		}
		return diff
	}
	return auxFloat(a) - auxFloat(b)
}

func auxBigInt(v any) (*big.Int, bool) {
	switch n := v.(type) {
	case int64:
		return big.NewInt(n), true
	case int:
		return big.NewInt(int64(n)), true
	case bool:
		if n {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case *big.Int:
		return n, true
	}
	return nil, false
}

func auxFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	if n, ok := auxBigInt(v); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f
	}
	panic("TypeError: unsupported operand type(s) for -: '" + pyrepr.TypeName(v) + "'")
}
