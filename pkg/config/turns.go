package config

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Iteration cap meaning no limit
const TurnLimitUnlimited = math.MaxInt

var unlimitedSpellings = map[string]bool{
	"none": true, "null": true, "unlimited": true, "infinite": true, "infinity": true, "inf": true, "\u221E": true, "-1": true, "0": true,
}

// Iteration cap of an integer, unlimited when not positive or beyond the Go int range
func turnCap(n int64) int {
	if n <= 0 || n > math.MaxInt {
		return TurnLimitUnlimited
	}
	return int(n)
}

// Iteration cap of a float cut toward zero, the fallback for a NaN and no limit for an infinity
func turnCapOfFloat(f float64, fallback int) int {
	if math.IsNaN(f) {
		return fallback
	}
	if t := math.Trunc(f); t > 0 && t < float64(math.MaxInt) {
		return int(t)
	}
	return TurnLimitUnlimited
}

// Iteration cap from a raw max_turns value, the fallback for null, a bool, a NaN and unparseable values
func ResolveTurnLimit(raw any, fallback int) int {
	if n, ok := anyutil.Int(raw); ok {
		return turnCap(n)
	}
	switch v := raw.(type) {
	case *big.Int:
		// Integers beyond int64 the YAML loader keeps whole
		if v != nil {
			return TurnLimitUnlimited
		}
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
		if n, err := strconv.ParseInt(s, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
			return turnCap(n)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return fallback
		}
		return turnCapOfFloat(f, fallback)
	}
	return fallback
}
