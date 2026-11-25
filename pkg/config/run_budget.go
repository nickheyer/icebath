package config

import (
	"math"
	"strconv"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Positive seconds of a run budget from a number of any Go kind or numeric text, nil for no budget: absent, a bool, unreadable, zero or less
func RunBudgetSeconds(value any) *float64 {
	var seconds float64
	switch v := value.(type) {
	case nil, bool:
		return nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil
		}
		seconds = f
	default:
		f, ok := anyutil.Float(v)
		if !ok {
			return nil
		}
		seconds = f
	}
	if math.IsNaN(seconds) || seconds <= 0 {
		return nil
	}
	return &seconds
}
