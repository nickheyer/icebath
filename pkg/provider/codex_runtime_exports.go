package provider

// RecordCodexAppServerUsage folds a codex turn's token usage into the agent, compressor and session row
func RecordCodexAppServerUsage(agent CodexRuntimeAgent, turn *CodexTurnResult, messages []map[string]any) map[string]any {
	return recordCodexAppServerUsage(agent, turn, messages)
}

// RecordCodexAppServerCompaction records a server side or forced codex compaction and reports whether one happened
func RecordCodexAppServerCompaction(agent CodexRuntimeAgent, turn *CodexTurnResult, approxTokens int, force bool) bool {
	return recordCodexAppServerCompaction(agent, turn, approxTokens, force)
}
