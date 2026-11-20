package state

// Index of this turn's user message in a plain transcript, minus one when no user turn remains
func ReanchorCurrentTurnUserIdx(messages []any, userMessage any) int {
	return reanchorCurrentTurnUserIdx(messages, userMessage)
}
