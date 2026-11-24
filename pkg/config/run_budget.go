package config

import (
	"math"
	"strconv"
	"strings"
)

// Positive seconds of a run budget, nil for no budget: absent, a bool, unreadable, zero or less
func RunBudgetSeconds(value any) *float64 {
	var seconds float64
	switch v := value.(type) {
	case nil, bool:
		return nil
	case int:
		seconds = float64(v)
	case int64:
		seconds = float64(v)
	case float64:
		seconds = v
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		seconds = f
	default:
		return nil
	}
	if math.IsNaN(seconds) || seconds <= 0 {
		return nil
	}
	return &seconds
}
