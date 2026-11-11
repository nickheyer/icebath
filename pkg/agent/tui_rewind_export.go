package agent

import "github.com/nickheyer/icebath/pkg/state"

// Installs a rewound TUI transcript, the flush cursor and scan prefix kept only for a persisted session
func (a *AIAgent) InstallRewoundHistory(history []map[string]any, persisted bool) {
	messages := make([]*state.Message, len(history))
	keys := make([]any, len(history))
	for i, m := range history {
		messages[i] = state.MessageFromDict(m)
		keys[i] = rowIdentity(m)
	}
	a.sessionMessages = messages
	if persisted {
		a.lastFlushedDBIdx = len(history)
		a.dbFlushScanPrefix = keys
		return
	}
	a.lastFlushedDBIdx = 0
	a.dbFlushScanPrefix = nil
}
