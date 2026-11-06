package state

// Synthetic compression scaffolding user row such as a continuation or todo injection
func IsSyntheticCompressionUserTurn(message map[string]any) bool {
	return isSyntheticCompressionUserTurn(message)
}

// Older handoff prefixes a summary body may still begin with
func HistoricalSummaryPrefixes() []string { return append([]string(nil), historicalSummaryPrefixes...) }

// Empty non summary user role echo
func IsBlankUserTurn(message map[string]any) bool { return isBlankUserTurn(message) }

// User row carrying input worth anchoring
func IsActionableUserTurn(message map[string]any) bool { return isActionableUserTurn(message) }
