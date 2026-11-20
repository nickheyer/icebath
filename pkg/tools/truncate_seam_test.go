package tools

// Drops the cached limits for tests or after a config hot reload
func ResetToolOutputLimitsCache() {
	outputLimitsMu.Lock()
	defer outputLimitsMu.Unlock()
	outputLimitsCache = map[string]OutputLimits{}
}
