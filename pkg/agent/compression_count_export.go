package agent

// Compactions the agent's context engine counted, false when the agent has no engine
func (a *AIAgent) ContextCompressionCount() (int, bool) {
	if a == nil || a.contextCompressor == nil {
		return 0, false
	}
	return a.contextCompressor.Base().CompressionCount, true
}

// Seeds the context engine's compaction count with the conversation's carried count
func (a *AIAgent) SeedContextCompressionCount(count int) {
	if a != nil && a.contextCompressor != nil {
		a.contextCompressor.Base().CompressionCount = count
	}
}
