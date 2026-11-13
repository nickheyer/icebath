package plugins

import (
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

// Go live notices an open chat holds, one per plugin key in queue order, plus those riding this tool batch
type PluginLiveNotes struct {
	keys    []string
	notes   map[string]string
	inBatch map[string]string
}

var liveNotesMu sync.Mutex

// Queued notices of the agent in plugin order, empty when it holds none
func queuedNotes(agent *PluginLiveNotes) ([]string, map[string]string) {
	if agent.notes == nil {
		return nil, map[string]string{}
	}
	return agent.keys, agent.notes
}

// Queues a note for the next tool result or user turn, replacing this plugin's older one
func QueuePluginNotice(agent *PluginLiveNotes, pluginKey, note string) {
	liveNotesMu.Lock()
	defer liveNotesMu.Unlock()
	queuedKeys, queued := queuedNotes(agent)
	keys := make([]string, 0, len(queuedKeys)+1)
	notes := make(map[string]string, len(queuedKeys)+1)
	for _, k := range queuedKeys {
		if k != pluginKey {
			keys = append(keys, k)
			notes[k] = queued[k]
		}
	}
	agent.keys = append(keys, pluginKey)
	notes[pluginKey] = note
	agent.notes = notes
}

// Every queued notice oldest plugin first joined by blank lines, clearing the queue
func PopPluginNotices(agent *PluginLiveNotes) string {
	liveNotesMu.Lock()
	keys, notes := queuedNotes(agent)
	if len(keys) > 0 {
		agent.keys, agent.notes = nil, map[string]string{}
	}
	liveNotesMu.Unlock()
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = notes[k]
	}
	return strings.Join(parts, "\n\n")
}

// Appends every queued notice after the content of the tool result about to be flushed
func AttachToToolMessage(agent *PluginLiveNotes, message *state.Message) {
	notes := PopPluginNotices(agent)
	if notes == "" {
		return
	}
	appendLiveNote(message, notes)
	merged := map[string]string{}
	for k, v := range agent.inBatch {
		merged[k] = v
	}
	merged[message.ToolCallID] = notes
	agent.inBatch = merged
}

// Puts a notice back on a batch row whose content the turn budget spilled to a file
func KeepAfterTurnBudget(agent *PluginLiveNotes, batch []*state.Message) {
	inBatch := agent.inBatch
	if len(inBatch) == 0 {
		return
	}
	agent.inBatch = map[string]string{}
	for _, message := range batch {
		notes := inBatch[message.ToolCallID]
		if notes != "" && !strings.Contains(state.FlattenMessageText(message.Content, "\n"), notes) {
			appendLiveNote(message, notes)
		}
	}
}

// Text part appended to list content, otherwise the notes after a blank line
func appendLiveNote(message *state.Message, notes string) {
	switch content := message.Content.(type) {
	case []any:
		message.Content = append(append([]any{}, content...), map[string]any{"type": "text", "text": notes})
	case []map[string]any:
		out := make([]any, 0, len(content)+1)
		for _, part := range content {
			out = append(out, part)
		}
		message.Content = append(out, map[string]any{"type": "text", "text": notes})
	default:
		prefix := ""
		if config.PyTruthy(message.Content) {
			prefix = config.PyStr(message.Content)
		}
		message.Content = prefix + "\n\n" + notes
	}
}
