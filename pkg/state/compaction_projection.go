package state

// Carrier fields a display projection drops, provider replay metadata the transcript view never shows
var compactionInternalFields = []string{
	"tool_calls", "finish_reason", "reasoning", "reasoning_content", "reasoning_details",
	"codex_reasoning_items", "codex_message_items",
}

// Authentic transcript content of a message as a fresh dict, nil for a pure compaction handoff
func ProjectCompactionMessageForDisplay(message any) map[string]any {
	m, ok := message.(map[string]any)
	if !ok {
		return nil
	}
	if !IsCompactionSummaryMessage(m) {
		return copyMessage(m)
	}
	projected := StripContextSummaryHandoffMessage(m)
	if projected == nil {
		return nil
	}
	projected = copyMessage(projected)
	for _, key := range compactionInternalFields {
		delete(projected, key)
	}
	delete(projected, "display_kind")
	return projected
}
