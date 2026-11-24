package tuiserver

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// Float of an environment knob, the default when it is absent, empty or malformed
func envFloat(name string, def float64) float64 {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
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
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return def
	}
	if value > math.MaxInt || value < math.MinInt {
		if value < 0 {
			return math.MinInt
		}
		return math.MaxInt
	}
	return int(value)
}
