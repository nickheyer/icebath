package agent

// Descriptor of an error that escaped the turn dispatcher, classified through the provider classifier when it is an API failure
func BuildErrorSurfaceFromException(err error, providerName, model string, apiKey any) map[string]any {
	return buildErrorSurfaceFromException(err, providerName, model, apiKey)
}
