package provider

// Stream events consumed with no callbacks into the response shape
func normalizeConverseStreamEvents(next bedrockEventSource) (*bedrockConverseResult, error) {
	out, _, err := streamConverseWithCallbacks(next, bedrockStreamCallbacks{})
	return out, err
}
