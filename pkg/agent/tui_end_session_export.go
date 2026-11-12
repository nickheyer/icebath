package agent

import "github.com/nickheyer/icebath/pkg/state"

// Whether closing the agent ends its stored session row
func (a *AIAgent) EndSessionOnClose() bool { return a.endSessionOnClose }

// Seeds the live transcript as never flushed, the state a hermes case builds by assigning _session_messages
func (a *AIAgent) SetUnflushedSessionMessagesForTests(history []map[string]any) {
	messages := make([]*state.Message, len(history))
	for i, m := range history {
		messages[i] = state.MessageFromDict(m)
	}
	a.shareSessionMessageOrigins(messages, history, false)
	a.sessionMessages = messages
	a.lastFlushedDBIdx = 0
	a.dbFlushScanPrefix = nil
}
