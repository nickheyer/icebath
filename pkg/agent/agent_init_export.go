package agent

// Seconds of an agent run budget, nil when the value is absent or not a positive number
func NormalizeRunBudgetSeconds(value any) *float64 { return normalizeRunBudgetSeconds(value) }
