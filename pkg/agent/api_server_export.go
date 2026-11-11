package agent

// Prompt tokens the session has spent
func (a *AIAgent) SessionPromptTokens() int { return a.sessionPromptTokens }

// Completion tokens the session has spent
func (a *AIAgent) SessionCompletionTokens() int { return a.sessionCompletionTokens }

// Total tokens the session has spent
func (a *AIAgent) SessionTotalTokens() int { return a.sessionTotalTokens }

// Cache read tokens the session has spent
func (a *AIAgent) SessionCacheReadTokens() int { return a.sessionCacheReadTokens }

// Cache write tokens the session has spent
func (a *AIAgent) SessionCacheWriteTokens() int { return a.sessionCacheWriteTokens }

// Adopts the transcript's unanswered tail row as this turn's user message instead of appending it again
func AdoptUnansweredTurn(history *[]map[string]any, query any, a *AIAgent) bool {
	return adoptUnansweredTurn(history, query, a)
}
