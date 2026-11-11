package activesessions

import (
	"fmt"
	"log/slog"
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Positive integer session cap of a config value, nil when disabled or invalid with a warning logged
func CoerceMaxConcurrentSessions(value any, key string) *big.Int {
	if value == nil {
		return nil
	}
	var parsed *big.Int
	var err error
	_, isBool := value.(bool)
	f, isFloat := value.(float64)
	switch {
	case isBool || (isFloat && (math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f))):
		err = fmt.Errorf("ValueError: %s", pyrepr.Repr(value))
	default:
		if s, ok := value.(string); ok {
			if n, good := pyint.Parse(pystr.Strip(s)); good {
				parsed = n
			} else {
				err = fmt.Errorf("ValueError: %s", s)
			}
		} else {
			parsed, err = pyint.Of(value)
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Ignoring invalid %s=%s (expected a positive integer; 0/null disables)", key, pyrepr.Repr(value)))
		return nil
	}
	if parsed.Sign() <= 0 {
		return nil
	}
	return parsed
}

// Top level max_concurrent_sessions of a config with the gateway section as the fallback
func ResolveMaxConcurrentSessions(config any) *big.Int {
	var raw any
	key := "max_concurrent_sessions"
	cfg, isDict := config.(map[string]any)
	if obj, ok := config.(*pyjson.Object); ok {
		cfg, isDict = obj.Vals, true
	}
	if isDict {
		if v, has := cfg["max_concurrent_sessions"]; has {
			raw = v
		} else {
			gw, ok := cfg["gateway"].(map[string]any)
			if obj, isObj := cfg["gateway"].(*pyjson.Object); isObj {
				gw, ok = obj.Vals, true
			}
			if ok {
				raw = gw["max_concurrent_sessions"]
				key = "gateway.max_concurrent_sessions"
			}
		}
	} else if attr, ok := config.(maxConcurrentSessionsAttr); ok {
		raw = attr.MaxConcurrentSessionsAttr()
	}
	return CoerceMaxConcurrentSessions(raw, key)
}

// Config object exposing its max_concurrent_sessions attribute, nil when unset
type maxConcurrentSessionsAttr interface{ MaxConcurrentSessionsAttr() any }
