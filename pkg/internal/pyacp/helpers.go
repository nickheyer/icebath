package pyacp

// Pointer to a copy of v for optional schema fields
func Ptr[T any](v T) *T { return &v }

// Text content block
func TextBlock(text string) *TextContentBlock { return &TextContentBlock{Text: text} }

// Tool call content wrapping a content block
func ToolContent(block ContentBlock) *ContentToolCallContent {
	return &ContentToolCallContent{Content: block}
}

// Diff tool call content, old text omitted when nil
func ToolDiffContent(path, newText string, oldText *string) *FileEditToolCallContent {
	return &FileEditToolCallContent{Path: path, NewText: newText, OldText: oldText}
}

// User message chunk update
func UpdateUserMessage(content ContentBlock) *UserMessageChunk {
	return &UserMessageChunk{Content: content}
}

// User message chunk update of plain text
func UpdateUserMessageText(text string) *UserMessageChunk {
	return UpdateUserMessage(TextBlock(text))
}

// Agent message chunk update
func UpdateAgentMessage(content ContentBlock) *AgentMessageChunk {
	return &AgentMessageChunk{Content: content}
}

// Agent message chunk update of plain text
func UpdateAgentMessageText(text string) *AgentMessageChunk {
	return UpdateAgentMessage(TextBlock(text))
}

// Agent thought chunk update
func UpdateAgentThought(content ContentBlock) *AgentThoughtChunk {
	return &AgentThoughtChunk{Content: content}
}

// Agent thought chunk update of plain text
func UpdateAgentThoughtText(text string) *AgentThoughtChunk {
	return UpdateAgentThought(TextBlock(text))
}

// Session notification carrying one update
func NewSessionNotification(sessionID string, update SessionUpdate) *SessionNotification {
	return &SessionNotification{SessionID: sessionID, Update: update}
}

// Optional fields of a tool call start or progress update
type ToolCallFields struct {
	Title     *string
	Kind      *string
	Status    *string
	Content   []ToolCallContent
	Locations []ToolCallLocation
	RawInput  any
	RawOutput any
}

// Tool call start update
func StartToolCall(toolCallID, title string, f ToolCallFields) *ToolCallStart {
	return &ToolCallStart{
		ToolCallID: toolCallID,
		Title:      title,
		Kind:       f.Kind,
		Status:     f.Status,
		Content:    f.Content,
		Locations:  f.Locations,
		RawInput:   f.RawInput,
		RawOutput:  f.RawOutput,
	}
}

// Tool call progress update
func UpdateToolCall(toolCallID string, f ToolCallFields) *ToolCallProgress {
	return &ToolCallProgress{
		ToolCallID: toolCallID,
		Title:      f.Title,
		Kind:       f.Kind,
		Status:     f.Status,
		Content:    f.Content,
		Locations:  f.Locations,
		RawInput:   f.RawInput,
		RawOutput:  f.RawOutput,
	}
}

// Plan entry with content, priority and status
func NewPlanEntry(content, priority, status string) PlanEntry {
	return PlanEntry{Content: content, Priority: priority, Status: status}
}

// Plan update of the given entries
func UpdatePlan(entries []PlanEntry) *AgentPlanUpdate {
	return &AgentPlanUpdate{Entries: entries}
}
