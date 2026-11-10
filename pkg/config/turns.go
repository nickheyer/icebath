package config

import (
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

// Iteration cap from a raw max_turns value, the fallback for null and unparseable values, a NaN or infinite float panicking as int() raises
func ResolveTurnLimit(raw any, fallback int) int {
	if _, isBool := raw.(bool); raw == nil || isBool {
		return fallback
	}
	if n, ok := pyint.Instance(raw); ok {
		return turnCap(n)
	}
	switch v := raw.(type) {
	case float64, float32:
		n, err := pyint.Of(v)
		if err != nil {
			panic(err)
		}
		return turnCap(n)
	case string:
		s := pystr.Lower(pystr.Strip(v))
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
		n, err := pyint.Of(f)
		if err != nil {
			if pe, isPy := err.(*pyint.Error); isPy && pe.Kind == "ValueError" {
				return fallback
			}
			panic(err)
		}
		return turnCap(n)
	}
	return fallback
}
