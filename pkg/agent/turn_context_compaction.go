package agent

import "github.com/nickheyer/icebath/pkg/state"

// Re-anchors this turn's user index after compaction so the stamp and persist override hit one row
func reanchor(a *AIAgent, messages []*state.Message, userMessage any) int {
	idx := reanchorCurrentTurnUserIdx(messages, userMessage)
	a.persistUserMessageIdx = &idx
	return idx
}
