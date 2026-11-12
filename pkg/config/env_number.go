package config

import (
	"math"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Bool-ish spellings the shared truthy check accepts after stripping and lowercasing
var truthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Whether a string is one of the shared truthy spellings once stripped and lowercased
func isTruthyString(value string) bool {
	return truthyStrings[pystr.Lower(pystr.Strip(value))]
}

// Stripped environment variable passed through cast, def when unset, blank or rejected by cast
func envNumber[T any](key string, def T, cast func(string) (T, bool)) T {
	raw := pystr.Strip(os.Getenv(key))
	if raw == "" {
		return def
	}
	value, ok := cast(raw)
	if !ok {
		return def
	}
	return value
}

// Integer value of an environment variable saturated to int64, def when unset, blank or no Python int literal
func EnvInt(key string, def int64) int64 {
	return envNumber(key, def, func(raw string) (int64, bool) {
		n, ok := pyint.Parse(raw)
		if !ok {
			return 0, false
		}
		if !n.IsInt64() {
			if n.Sign() > 0 {
				return math.MaxInt64, true
			}
			return math.MinInt64, true
		}
		return n.Int64(), true
	})
}

// Float value of an environment variable, def when unset, blank or no Python float literal
func EnvFloat(key string, def float64) float64 {
	return envNumber(key, def, func(raw string) (float64, bool) {
		f, err := pyint.ParseFloat(raw)
		return f, err == nil
	})
}

// Whether an environment variable holds a truthy spelling, an unset variable reading as false
func EnvBool(key string) bool {
	return isTruthyString(os.Getenv(key))
}
