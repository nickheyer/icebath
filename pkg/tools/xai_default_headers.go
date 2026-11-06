package tools

// Headers for SDK and raw HTTP clients talking to xAI, replacing the SDK user agent
func XAIDefaultHeaders() map[string]string {
	return map[string]string{"User-Agent": icebathXAIUserAgent()}
}
