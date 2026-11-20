package agent

// Emits or discards the deferred context engine compression notification, repeated calls are no ops
func FinalizeContextEngineCompressionNotification(a *AIAgent, committed bool) bool {
	return finalizeContextEngineCompressionNotification(a, committed)
}
