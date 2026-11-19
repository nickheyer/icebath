package provider

// ResetBedrockCachePointRejections forgets every recorded cachePoint verdict
func ResetBedrockCachePointRejections() {
	bedrockCachePointMu.Lock()
	defer bedrockCachePointMu.Unlock()
	bedrockCachePointRejections = map[string]map[string]bool{}
}

// Converse inputs with the default 4096 token output limit
func newBedrockConverseInput(model string, messages []map[string]any) bedrockConverseInput {
	maxTokens := bedrockDefaultMaxTokens
	return bedrockConverseInput{Model: model, Messages: messages, MaxTokens: &maxTokens}
}
