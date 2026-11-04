package config

import (
	"math"
	"strconv"
	"strings"
)

// Iteration cap meaning no limit
const TurnLimitUnlimited = math.MaxInt

var unlimitedSpellings = map[string]bool{
	"none": true, "null": true, "unlimited": true, "infinite": true, "infinity": true, "inf": true, "-1": true, "0": true,
}

// Iteration cap from a raw max_turns value, the fallback for null and unparseable values
func ResolveTurnLimit(raw any, fallback int) int {
	var n int
	switch v := raw.(type) {
	case nil, bool:
		return fallback
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			return fallback
		}
		if unlimitedSpellings[s] {
			return TurnLimitUnlimited
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			f, ferr := strconv.ParseFloat(s, 64)
			if ferr != nil {
				return fallback
			}
			i = int(f)
		}
		n = i
	default:
		return fallback
	}
	if n <= 0 {
		return TurnLimitUnlimited
	}
	return n
}
