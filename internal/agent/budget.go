package agent

import (
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

type IterationBudget struct {
	mu   sync.Mutex
	max  int
	used int
}

func NewIterationBudget(max int) *IterationBudget {
	return &IterationBudget{max: max}
}

func (b *IterationBudget) Consume() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

func (b *IterationBudget) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 {
		b.used--
	}
}

func (b *IterationBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *IterationBudget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(0, b.max-b.used)
}

func (b *IterationBudget) Max() int {
	return b.max
}

type BudgetState struct {
	Used, Max  int
	RunElapsed time.Duration
	RunBudget  time.Duration
	GraceCall  bool
}

const (
	defaultAPIMaxRetries      = 3
	defaultAutoRecoveryCycles = 5
	maxCompressionAttempts    = 3
	maxOuterLoopErrors        = 8
	turnLeaseTTL              = 300 * time.Second
	runBudgetWrapUpRatio      = 0.8
	activityPersistInterval   = 60 * time.Second
)

type limits struct {
	apiMaxRetries          int
	autoRecoveryCycles     int
	budgetWarningRatio     float64
	budgetWarning          bool
	runBudget              time.Duration
	maxCompressionAttempts int
	maxOuterLoopErrors     int
	turnLeaseTTL           time.Duration
}

func normalizeBudgetWarningRatio(v *float64) (float64, bool) {
	if v == nil {
		return 0, false
	}
	ratio := *v
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio >= 1 {
		return 0, false
	}
	return ratio, true
}

func normalizeRunBudget(seconds *float64) time.Duration {
	if seconds == nil || math.IsNaN(*seconds) || *seconds <= 0 {
		return 0
	}
	if math.IsInf(*seconds, 1) || *seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(*seconds * float64(time.Second))
}

func limitsFrom(cfg *config.Config, runBudget time.Duration) limits {
	l := limits{
		apiMaxRetries:          defaultAPIMaxRetries,
		autoRecoveryCycles:     defaultAutoRecoveryCycles,
		runBudget:              runBudget,
		maxCompressionAttempts: maxCompressionAttempts,
		maxOuterLoopErrors:     maxOuterLoopErrors,
		turnLeaseTTL:           turnLeaseTTL,
	}
	if cfg == nil {
		return l
	}
	l.apiMaxRetries = max(cfg.Agent.APIMaxRetries, 1)
	l.autoRecoveryCycles = max(cfg.Agent.AutoRecoveryCycles, 0)
	l.budgetWarningRatio, l.budgetWarning = normalizeBudgetWarningRatio(cfg.Agent.BudgetWarningRatio)
	if l.runBudget <= 0 {
		l.runBudget = normalizeRunBudget(cfg.Agent.RunBudgetSeconds)
	}
	return l
}
