package config

import (
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Iteration cap meaning no limit
const TurnLimitUnlimited = math.MaxInt

var unlimitedSpellings = map[string]bool{
	"none": true, "null": true, "unlimited": true, "infinite": true, "infinity": true, "inf": true, "\u221E": true, "-1": true, "0": true,
}

// Iteration cap of a Python int, unlimited when not positive or beyond the Go int range
func turnCap(n *big.Int) int {
	if n.Sign() <= 0 || !n.IsInt64() || n.Int64() > math.MaxInt {
		return TurnLimitUnlimited
	}
	return int(n.Int64())
}

// Iteration cap of a float, the fallback for a NaN and no limit for an infinity
func turnCapOfFloat(f float64, fallback int) int {
	if math.IsNaN(f) {
		return fallback
	}
	if math.IsInf(f, 0) {
		return TurnLimitUnlimited
	}
	n, err := pyint.Of(f)
	if err != nil {
		return fallback
	}
	return turnCap(n)
}

// Iteration cap from a raw max_turns value, the fallback for null, a NaN and unparseable values
func ResolveTurnLimit(raw any, fallback int) int {
	if _, isBool := raw.(bool); raw == nil || isBool {
		return fallback
	}
	if n, ok := pyint.Instance(raw); ok {
		return turnCap(n)
	}
	switch v := raw.(type) {
	case float64:
		return turnCapOfFloat(v, fallback)
	case float32:
		return turnCapOfFloat(float64(v), fallback)
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			return fallback
		}
		if unlimitedSpellings[s] {
			return TurnLimitUnlimited
		}
		if n, ok := pyint.Parse(s); ok {
			return turnCap(n)
		}
		f, err := pyint.ParseFloat(s)
		if err != nil {
			return fallback
		}
		return turnCapOfFloat(f, fallback)
	}
	return fallback
}
