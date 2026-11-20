package agent

// Descriptor for a failed turn result the gateway settles, nil when the result carries no failure signal
func BuildErrorSurfaceFromResult(result any, providerName, model string) map[string]any {
	return buildErrorSurfaceFromResult(result, providerName, model)
}
