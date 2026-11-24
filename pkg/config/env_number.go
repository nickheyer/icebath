package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Bool-ish spellings the shared truthy check accepts after stripping and lowercasing
var truthyStrings = map[string]bool{"1": true, "true": true, "yes": true, "on": true}

// Whether a string is one of the shared truthy spellings once stripped and lowercased
func isTruthyString(value string) bool {
	return truthyStrings[strings.ToLower(strings.TrimSpace(value))]
}

// Stripped environment variable passed through cast, def when unset, blank or rejected by cast
func envNumber[T any](key string, def T, cast func(string) (T, bool)) T {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	value, ok := cast(raw)
	if !ok {
		return def
	}
	return value
}

// Integer value of an environment variable saturated to int64, def when unset, blank or not a decimal integer
func EnvInt(key string, def int64) int64 {
	return envNumber(key, def, func(raw string) (int64, bool) {
		n, err := strconv.ParseInt(raw, 10, 64)
		return n, err == nil || errors.Is(err, strconv.ErrRange)
	})
}

// Float value of an environment variable, an infinity for one too large, def when unset, blank or not a number
func EnvFloat(key string, def float64) float64 {
	return envNumber(key, def, func(raw string) (float64, bool) {
		f, err := strconv.ParseFloat(raw, 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	})
}
