package agent

import (
	"math/big"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Seeds the running cost of the current empty response streak
func (a *AIAgent) SetEmptyStreakCostUSD(cost *big.Rat) { a.emptyStreakCostUSD = cost }

// Number of retry status lines still buffered for the next flush
func (a *AIAgent) RetryStatusBufferLen() int { return len(a.retryStatusBuffer) }

// Terminal empty delivery text with the rows the empty sentinel landed on, retries and fallback exhausted
func (a *AIAgent) TerminalEmptyTurn(messages []map[string]any, finishReason string) (string, []map[string]any) {
	s := &LoopState{Messages: rowsToMessages(messages)}
	final := terminalEmpty(a, s, &provider.Response{Message: &state.Message{Role: "assistant"}}, finishReason)
	return final, messagesToRows(s.Messages)
}
