package tools

// Names of the fuzzy matcher strategies in chain order, the file edit metric's strategy vocabulary
func FuzzyStrategyNames() []string {
	out := make([]string, 0, len(fuzzyStrategies))
	for _, st := range fuzzyStrategies {
		out = append(out, st.name)
	}
	return out
}

// Gateway error family of a connector execute error code, empty when the code is unknown
func ExecuteErrorFamily(code, family string) string {
	return executeErrorFamily(code, family)
}
