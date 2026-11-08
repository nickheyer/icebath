package agent

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// ErrUserTurnIndex and ErrNotUserOriginatedTurn are the IndexError and ValueError of the rewind helper
var (
	ErrUserTurnIndex         = state.ErrUserTurnIndex
	ErrNotUserOriginatedTurn = state.ErrNotUserOriginatedTurn
)

// Live human authored projection of a user row, nil when the row carries none
func UserOriginatedTurnView(message any) map[string]any { return state.UserOriginatedTurnView(message) }

// Rewind prefix and canonical live view for index, a composite carrier keeping its handoff at the head
func HistoryBeforeUserOriginatedTurn(messages []map[string]any, index int) ([]map[string]any, map[string]any, error) {
	return state.HistoryBeforeUserOriginatedTurn(messages, index)
}

// Summary bearing row that still carries a live user ask
func handoffCarriesLiveUserContent(message any) bool {
	m, ok := message.(map[string]any)
	return ok && m != nil && state.StripContextSummaryHandoffMessage(m) != nil
}

// Next model call driven only by a handoff, trailing tool rows meaning an exchange is in flight
func referenceHandoffWouldDriveNextModelCall(messages []map[string]any) bool {
	if len(messages) == 0 {
		return false
	}
	lastDriving := -1
	for index, message := range messages {
		if !state.IsCompactionSummaryMessage(message) {
			continue
		}
		mergedCompletedAssistant := message != nil && message["role"] == "assistant" &&
			state.ClassifySummaryContent(message["content"]) == "merged" &&
			message["finish_reason"] == "stop" && !pyrepr.Truthy(message["tool_calls"])
		if !(handoffCarriesLiveUserContent(message) && !mergedCompletedAssistant) {
			lastDriving = index
		}
	}
	if lastDriving < 0 {
		return false
	}
	for _, message := range messages[lastDriving+1:] {
		if message == nil {
			continue
		}
		role := message["role"]
		if role == "tool" || (role == "assistant" && pyrepr.Truthy(message["tool_calls"])) || state.IsRealUserTurn(message) ||
			(state.IsCompactionSummaryMessage(message) && handoffCarriesLiveUserContent(message)) {
			return false
		}
	}
	return true
}

// Human authored user turn rather than compaction scaffolding, the check dispatchers use instead of the role
func IsUserOriginatedTurn(message any) bool { return UserOriginatedTurnView(message) != nil }
