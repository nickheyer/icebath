package acputil

import (
	"encoding/json"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Change to a session the agent reports: *UserMessageChunk, *AgentMessageChunk, *AgentThoughtChunk, *ToolCallStart, *ToolCallProgress, *AgentPlanUpdate, *AvailableCommandsUpdate, *CurrentModeUpdate, *ConfigOptionUpdate, *SessionInfoUpdate or *UsageUpdate
type SessionUpdate interface{ isSessionUpdate() }

func (*UserMessageChunk) isSessionUpdate()        {}
func (*AgentMessageChunk) isSessionUpdate()       {}
func (*AgentThoughtChunk) isSessionUpdate()       {}
func (*ToolCallStart) isSessionUpdate()           {}
func (*ToolCallProgress) isSessionUpdate()        {}
func (*AgentPlanUpdate) isSessionUpdate()         {}
func (*AvailableCommandsUpdate) isSessionUpdate() {}
func (*CurrentModeUpdate) isSessionUpdate()       {}
func (*ConfigOptionUpdate) isSessionUpdate()      {}
func (*SessionInfoUpdate) isSessionUpdate()       {}
func (*UsageUpdate) isSessionUpdate()             {}

// Params of session/update: one update to one session
type SessionNotification struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	SessionID string           `json:"sessionId"`
	Update    SessionUpdate    `json:"update"`
}

// Fills the notification from its JSON object, which must carry sessionId and update
func (n *SessionNotification) UnmarshalJSON(data []byte) error {
	type plain SessionNotification
	var w struct {
		*plain
		Update json.RawMessage `json:"update"`
	}
	w.plain = (*plain)(n)
	if err := decodeObject(data, &w, "SessionNotification", "sessionId", "update"); err != nil {
		return err
	}
	update, err := decodeSessionUpdate(w.Update)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	n.Update = update
	return nil
}

// Chunk of a user message, replayed from history or echoed back
type UserMessageChunk struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Content   ContentBlock     `json:"content"`
	MessageID *string          `json:"messageId,omitempty"`
}

// JSON object of the chunk with sessionUpdate "user_message_chunk"
func (c UserMessageChunk) MarshalJSON() ([]byte, error) {
	type plain UserMessageChunk
	return withTag(plain(c), "sessionUpdate", "user_message_chunk")
}

// Fills the chunk from its JSON object, which must carry content
func (c *UserMessageChunk) UnmarshalJSON(data []byte) error {
	type plain UserMessageChunk
	var w struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	w.plain = (*plain)(c)
	if err := decodeObject(data, &w, "UserMessageChunk", "content"); err != nil {
		return err
	}
	block, err := decodeContentBlock(w.Content)
	if err != nil {
		return fmt.Errorf("content: %w", err)
	}
	c.Content = block
	return nil
}

// Chunk of the agent's reply
type AgentMessageChunk struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Content   ContentBlock     `json:"content"`
	MessageID *string          `json:"messageId,omitempty"`
}

// JSON object of the chunk with sessionUpdate "agent_message_chunk"
func (c AgentMessageChunk) MarshalJSON() ([]byte, error) {
	type plain AgentMessageChunk
	return withTag(plain(c), "sessionUpdate", "agent_message_chunk")
}

// Fills the chunk from its JSON object, which must carry content
func (c *AgentMessageChunk) UnmarshalJSON(data []byte) error {
	type plain AgentMessageChunk
	var w struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	w.plain = (*plain)(c)
	if err := decodeObject(data, &w, "AgentMessageChunk", "content"); err != nil {
		return err
	}
	block, err := decodeContentBlock(w.Content)
	if err != nil {
		return fmt.Errorf("content: %w", err)
	}
	c.Content = block
	return nil
}

// Chunk of the agent's reasoning
type AgentThoughtChunk struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Content   ContentBlock     `json:"content"`
	MessageID *string          `json:"messageId,omitempty"`
}

// JSON object of the chunk with sessionUpdate "agent_thought_chunk"
func (c AgentThoughtChunk) MarshalJSON() ([]byte, error) {
	type plain AgentThoughtChunk
	return withTag(plain(c), "sessionUpdate", "agent_thought_chunk")
}

// Fills the chunk from its JSON object, which must carry content
func (c *AgentThoughtChunk) UnmarshalJSON(data []byte) error {
	type plain AgentThoughtChunk
	var w struct {
		*plain
		Content json.RawMessage `json:"content"`
	}
	w.plain = (*plain)(c)
	if err := decodeObject(data, &w, "AgentThoughtChunk", "content"); err != nil {
		return err
	}
	block, err := decodeContentBlock(w.Content)
	if err != nil {
		return fmt.Errorf("content: %w", err)
	}
	c.Content = block
	return nil
}

// The agent's whole plan, replacing the one reported before
type AgentPlanUpdate struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Entries []PlanEntry      `json:"entries"`
}

// JSON object of the plan with sessionUpdate "plan"
func (u AgentPlanUpdate) MarshalJSON() ([]byte, error) {
	type plain AgentPlanUpdate
	return withTag(plain(u), "sessionUpdate", "plan")
}

// Fills the plan from its JSON object, which must carry entries
func (u *AgentPlanUpdate) UnmarshalJSON(data []byte) error {
	type plain AgentPlanUpdate
	return decodeObject(data, (*plain)(u), "AgentPlanUpdate", "entries")
}

// One step of an agent plan
type PlanEntry struct {
	Meta    *jsonutil.Object `json:"_meta,omitempty"`
	Content string           `json:"content"`
	// high, medium or low
	Priority string `json:"priority"`
	// pending, in_progress or completed
	Status string `json:"status"`
}

// Fills the entry from its JSON object, which must carry content, priority and status
func (e *PlanEntry) UnmarshalJSON(data []byte) error {
	type plain PlanEntry
	return decodeObject(data, (*plain)(e), "PlanEntry", "content", "priority", "status")
}

// Commands the agent offers in the session, replacing those reported before
type AvailableCommandsUpdate struct {
	Meta              *jsonutil.Object   `json:"_meta,omitempty"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

// JSON object of the commands with sessionUpdate "available_commands_update"
func (u AvailableCommandsUpdate) MarshalJSON() ([]byte, error) {
	type plain AvailableCommandsUpdate
	return withTag(plain(u), "sessionUpdate", "available_commands_update")
}

// Fills the update from its JSON object, which must carry availableCommands
func (u *AvailableCommandsUpdate) UnmarshalJSON(data []byte) error {
	type plain AvailableCommandsUpdate
	return decodeObject(data, (*plain)(u), "AvailableCommandsUpdate", "availableCommands")
}

// Slash command the agent offers
type AvailableCommand struct {
	Meta        *jsonutil.Object          `json:"_meta,omitempty"`
	Description string                    `json:"description"`
	Input       *UnstructuredCommandInput `json:"input,omitempty"`
	Name        string                    `json:"name"`
}

// Fills the command from its JSON object, which must carry description and name
func (c *AvailableCommand) UnmarshalJSON(data []byte) error {
	type plain AvailableCommand
	return decodeObject(data, (*plain)(c), "AvailableCommand", "description", "name")
}

// Free text a command takes, with the hint shown while it is empty
type UnstructuredCommandInput struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	Hint string           `json:"hint"`
}

// Fills the input from its JSON object, which must carry hint
func (i *UnstructuredCommandInput) UnmarshalJSON(data []byte) error {
	type plain UnstructuredCommandInput
	return decodeObject(data, (*plain)(i), "UnstructuredCommandInput", "hint")
}

// Mode the session switched to
type CurrentModeUpdate struct {
	Meta          *jsonutil.Object `json:"_meta,omitempty"`
	CurrentModeID string           `json:"currentModeId"`
}

// JSON object of the update with sessionUpdate "current_mode_update"
func (u CurrentModeUpdate) MarshalJSON() ([]byte, error) {
	type plain CurrentModeUpdate
	return withTag(plain(u), "sessionUpdate", "current_mode_update")
}

// Fills the update from its JSON object, which must carry currentModeId
func (u *CurrentModeUpdate) UnmarshalJSON(data []byte) error {
	type plain CurrentModeUpdate
	return decodeObject(data, (*plain)(u), "CurrentModeUpdate", "currentModeId")
}

// Every config option of the session after one of them changed
type ConfigOptionUpdate struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

// JSON object of the update with sessionUpdate "config_option_update"
func (u ConfigOptionUpdate) MarshalJSON() ([]byte, error) {
	type plain ConfigOptionUpdate
	return withTag(plain(u), "sessionUpdate", "config_option_update")
}

// Fills the update from its JSON object, which must carry configOptions
func (u *ConfigOptionUpdate) UnmarshalJSON(data []byte) error {
	type plain ConfigOptionUpdate
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(u)
	if err := decodeObject(data, &w, "ConfigOptionUpdate", "configOptions"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	u.ConfigOptions = options
	return err
}

// New title or update time of the session
type SessionInfoUpdate struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Title     *string          `json:"title,omitempty"`
	UpdatedAt *string          `json:"updatedAt,omitempty"`
}

// JSON object of the update with sessionUpdate "session_info_update"
func (u SessionInfoUpdate) MarshalJSON() ([]byte, error) {
	type plain SessionInfoUpdate
	return withTag(plain(u), "sessionUpdate", "session_info_update")
}

// Context window use of the session and what it cost so far
type UsageUpdate struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	Cost *Cost            `json:"cost,omitempty"`
	// Tokens the context window holds
	Size int `json:"size"`
	// Tokens in use
	Used int `json:"used"`
}

// JSON object of the update with sessionUpdate "usage_update"
func (u UsageUpdate) MarshalJSON() ([]byte, error) {
	type plain UsageUpdate
	return withTag(plain(u), "sessionUpdate", "usage_update")
}

// Fills the update from its JSON object, which must carry size and used
func (u *UsageUpdate) UnmarshalJSON(data []byte) error {
	type plain UsageUpdate
	return decodeObject(data, (*plain)(u), "UsageUpdate", "size", "used")
}

// Amount of money in a currency
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Fills the cost from its JSON object, which must carry amount and currency
func (c *Cost) UnmarshalJSON(data []byte) error {
	type plain Cost
	return decodeObject(data, (*plain)(c), "Cost", "amount", "currency")
}

// Session update of a JSON object, the variant chosen by its sessionUpdate
func decodeSessionUpdate(data []byte) (SessionUpdate, error) {
	kind, ok := tagOf(data, "sessionUpdate")
	if !ok {
		return nil, &MissingFieldError{Type: "SessionUpdate", Field: "sessionUpdate"}
	}
	var update SessionUpdate
	switch kind {
	case "user_message_chunk":
		update = new(UserMessageChunk)
	case "agent_message_chunk":
		update = new(AgentMessageChunk)
	case "agent_thought_chunk":
		update = new(AgentThoughtChunk)
	case "tool_call":
		update = new(ToolCallStart)
	case "tool_call_update":
		update = new(ToolCallProgress)
	case "plan":
		update = new(AgentPlanUpdate)
	case "available_commands_update":
		update = new(AvailableCommandsUpdate)
	case "current_mode_update":
		update = new(CurrentModeUpdate)
	case "config_option_update":
		update = new(ConfigOptionUpdate)
	case "session_info_update":
		update = new(SessionInfoUpdate)
	case "usage_update":
		update = new(UsageUpdate)
	default:
		return nil, &UnknownVariantError{Union: "SessionUpdate", Key: "sessionUpdate", Value: kind}
	}
	if err := json.Unmarshal(data, update); err != nil {
		return nil, err
	}
	return update, nil
}
