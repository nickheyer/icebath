package activesessions

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Positive integer session cap of a config value, nil when disabled or invalid with a warning logged
func CoerceMaxConcurrentSessions(value any, key string) *big.Int {
	if value == nil {
		return nil
	}
	var parsed int64
	var err error
	_, isBool := value.(bool)
	f, isFloat := value.(float64)
	switch {
	case isBool || (isFloat && (math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f))):
		err = fmt.Errorf("%v is not a whole number", value)
	default:
		if s, ok := value.(string); ok {
			if parsed, err = strconv.ParseInt(strings.TrimSpace(s), 10, 64); err != nil {
				err = fmt.Errorf("%s is not a whole number", s)
			}
		} else {
			parsed, err = anyutil.ToInt(value)
		}
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("Ignoring invalid %s=%s (expected a positive integer; 0/null disables)", key, anyutil.Quote(value)))
		return nil
	}
	if parsed <= 0 {
		return nil
	}
	return big.NewInt(parsed)
}

// Top level max_concurrent_sessions of a config with the gateway section as the fallback
func ResolveMaxConcurrentSessions(config any) *big.Int {
	var raw any
	key := "max_concurrent_sessions"
	cfg, isDict := config.(map[string]any)
	if obj, ok := config.(*jsonutil.Object); ok {
		cfg, isDict = obj.Map(), true
	}
	if isDict {
		if v, has := cfg["max_concurrent_sessions"]; has {
			raw = v
		} else {
			gw, ok := cfg["gateway"].(map[string]any)
			if obj, isObj := cfg["gateway"].(*jsonutil.Object); isObj {
				gw, ok = obj.Map(), true
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
