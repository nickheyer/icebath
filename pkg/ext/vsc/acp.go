package vsc

// Protocol version the agent answers at initialize
const ProtocolVersion = 1

// Method names, the shared ones spelled as the Agent Client Protocol spells them
const (
	MethodInitialize   = "initialize"
	MethodLoadSession  = "session/load"
	MethodPrompt       = "session/prompt"
	MethodCancel       = "session/cancel"
	MethodSetMode      = "session/set_mode"
	MethodUpdate       = "session/update"
	MethodSessionsList = "_icebath/sessions/list"
	MethodState        = "_icebath/state"
)

// Reply to initialize naming the session on the TUI stage and the modes it may run in
type InitializeResponse struct {
	ProtocolVersion int       `json:"protocolVersion"`
	SessionID       string    `json:"sessionId"`
	Version         string    `json:"version"`
	Pid             int       `json:"pid"`
	Modes           ModeState `json:"modes"`
}

// Request to open a stored session in the TUI
type LoadSessionRequest struct {
	SessionID string `json:"sessionId"`
}

// Reply to a load carrying the modes
type LoadSessionResponse struct {
	Modes ModeState `json:"modes"`
}

// Modes a session may run in and the one in force
type ModeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []Mode `json:"availableModes"`
}

// One permission mode
type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Request to run a prompt on a session
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// One block of a prompt, text being the only kind the extension sends
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Reply naming why the run stopped
type PromptResponse struct {
	StopReason string `json:"stopReason"`
}

// Stop reasons
const (
	StopEndTurn   = "end_turn"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	StopCancelled = "cancelled"
)

// Notification cancelling the run of a session
type CancelNotification struct {
	SessionID string `json:"sessionId"`
}

// Request to change the mode
type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// Session update notification wrapping one variant
type SessionNotification struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// One change to a session, a file change proposed or made or the mode in force
type SessionUpdate struct {
	SessionUpdate string            `json:"sessionUpdate"`
	ToolCallID    string            `json:"toolCallId,omitempty"`
	Status        string            `json:"status,omitempty"`
	Content       []ToolCallContent `json:"content,omitempty"`
	CurrentModeID string            `json:"currentModeId,omitempty"`
}

// Update variant names
const (
	UpdateToolCall    = "tool_call"
	UpdateToolCallMod = "tool_call_update"
	UpdateModeChanged = "current_mode_update"
)

// Tool call statuses, pending while the TUI asks, then completed or failed
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Diff body of a file change, oldText null when the file did not exist
type ToolCallContent struct {
	Type    string  `json:"type"`
	Path    string  `json:"path"`
	OldText *string `json:"oldText"`
	NewText string  `json:"newText"`
}

// Reply listing stored sessions newest first
type SessionsListResponse struct {
	Sessions []SessionEntry `json:"sessions"`
}

// One stored session in a listing
type SessionEntry struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Updated   string `json:"updated"`
	Busy      bool   `json:"busy"`
}

// Notification describing the run state of a session
type StateNotification struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
	Running   bool   `json:"running"`
	Waiting   bool   `json:"waiting"`
	Current   bool   `json:"current"`
}
