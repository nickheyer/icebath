package agent

import (
	"math"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// A finite ratio strictly between zero and one with true, false when the budget warning is off
func NormalizeBudgetWarningRatio(value any) (float64, bool) {
	var ratio float64
	switch v := value.(type) {
	case nil, bool:
		return 0, false
	case float64:
		ratio = v
	case float32:
		ratio = float64(v)
	case int:
		ratio = float64(v)
	case int64:
		ratio = float64(v)
	case int32:
		ratio = float64(v)
	case uint64:
		ratio = float64(v)
	case string:
		f, err := pyint.ParseFloat(v)
		if err != nil {
			return 0, false
		}
		ratio = f
	default:
		return 0, false
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio >= 1 {
		return 0, false
	}
	return ratio, true
}

// Thread safe iteration counter where refunded execute_code iterations do not eat into the budget
type IterationBudget struct {
	MaxTotal int
	used     int
	mu       sync.Mutex
}

// Budget allowing maxTotal iterations
func NewIterationBudget(maxTotal int) *IterationBudget {
	return &IterationBudget{MaxTotal: maxTotal}
}

// Takes one iteration and reports whether the cap allowed it
func (b *IterationBudget) Consume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.MaxTotal {
		return false
	}
	b.used++
	return true
}

// Gives back one iteration when any were used
func (b *IterationBudget) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 {
		b.used--
	}
}

// Iterations consumed so far
func (b *IterationBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Iterations left before the cap, never negative
func (b *IterationBudget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.MaxTotal-b.used)
}
