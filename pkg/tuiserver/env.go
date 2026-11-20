package tuiserver

import (
	"math"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Float of an environment knob, the default when it is absent, empty or malformed
func envFloat(name string, def float64) float64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	value, err := pyint.ParseFloat(raw)
	if err != nil {
		return def
	}
	return value
}

// Integer of an environment knob, the default when absent, empty or malformed, saturating past the int range
func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	value, ok := pyint.Parse(raw)
	if !ok {
		return def
	}
	if !value.IsInt64() || value.Int64() > math.MaxInt || value.Int64() < math.MinInt {
		if value.Sign() < 0 {
			return math.MinInt
		}
		return math.MaxInt
	}
	return int(value.Int64())
}
