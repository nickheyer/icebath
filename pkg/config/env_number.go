package config

import (
	"math"
	"os"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Integer value of an environment variable saturated to int64, def when unset, blank or no Python int literal
func EnvInt(key string, def int64) int64 {
	raw := pystr.Strip(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, ok := pyint.Parse(raw)
	if !ok {
		return def
	}
	if !n.IsInt64() {
		if n.Sign() > 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return n.Int64()
}
