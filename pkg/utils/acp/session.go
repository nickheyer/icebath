package acputil

import (
	"encoding/json"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Params of session/new: the working directory and the MCP servers the client offers
type NewSessionRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	Cwd        string           `json:"cwd"`
	MCPServers []MCPServer      `json:"mcpServers"`
}

// Fills the request from its JSON object, which must carry cwd and mcpServers
func (r *NewSessionRequest) UnmarshalJSON(data []byte) error {
	type plain NewSessionRequest
	var w struct {
		*plain
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "NewSessionRequest", "cwd", "mcpServers"); err != nil {
		return err
	}
	servers, err := decodeEach(w.MCPServers, "mcpServers", decodeMCPServer)
	r.MCPServers = servers
	return err
}

// Result of session/new: the session with its models, modes and config options
type NewSessionResponse struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitzero"`
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	SessionID     string                `json:"sessionId"`
}

// Fills the response from its JSON object, which must carry sessionId
func (r *NewSessionResponse) UnmarshalJSON(data []byte) error {
	type plain NewSessionResponse
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "NewSessionResponse", "sessionId"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	r.ConfigOptions = options
	return err
}

// Params of session/load: the session, the working directory and the MCP servers the client offers
type LoadSessionRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	Cwd        string           `json:"cwd"`
	MCPServers []MCPServer      `json:"mcpServers"`
	SessionID  string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry cwd, mcpServers and sessionId
func (r *LoadSessionRequest) UnmarshalJSON(data []byte) error {
	type plain LoadSessionRequest
	var w struct {
		*plain
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "LoadSessionRequest", "cwd", "mcpServers", "sessionId"); err != nil {
		return err
	}
	servers, err := decodeEach(w.MCPServers, "mcpServers", decodeMCPServer)
	r.MCPServers = servers
	return err
}

// Result of session/load: the session's models, modes and config options
type LoadSessionResponse struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitzero"`
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
}

// Fills the response from its JSON object
func (r *LoadSessionResponse) UnmarshalJSON(data []byte) error {
	type plain LoadSessionResponse
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "LoadSessionResponse"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	r.ConfigOptions = options
	return err
}

// Params of session/list: a page cursor and a working directory to filter by
type ListSessionsRequest struct {
	Meta   *jsonutil.Object `json:"_meta,omitempty"`
	Cursor *string          `json:"cursor,omitempty"`
	Cwd    *string          `json:"cwd,omitempty"`
}

// Result of session/list: one page of sessions and the cursor of the next
type ListSessionsResponse struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	NextCursor *string          `json:"nextCursor,omitempty"`
	Sessions   []SessionInfo    `json:"sessions"`
}

// Fills the response from its JSON object, which must carry sessions
func (r *ListSessionsResponse) UnmarshalJSON(data []byte) error {
	type plain ListSessionsResponse
	return decodeObject(data, (*plain)(r), "ListSessionsResponse", "sessions")
}

// Session in a session list
type SessionInfo struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	Cwd       string           `json:"cwd"`
	SessionID string           `json:"sessionId"`
	Title     *string          `json:"title,omitempty"`
	UpdatedAt *string          `json:"updatedAt,omitempty"`
}

// Fills the session from its JSON object, which must carry cwd and sessionId
func (s *SessionInfo) UnmarshalJSON(data []byte) error {
	type plain SessionInfo
	return decodeObject(data, (*plain)(s), "SessionInfo", "cwd", "sessionId")
}

// Params of session/fork: the session to fork, the working directory and the MCP servers the client offers
type ForkSessionRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	Cwd        string           `json:"cwd"`
	MCPServers []MCPServer      `json:"mcpServers,omitzero"`
	SessionID  string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry cwd and sessionId
func (r *ForkSessionRequest) UnmarshalJSON(data []byte) error {
	type plain ForkSessionRequest
	var w struct {
		*plain
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "ForkSessionRequest", "cwd", "sessionId"); err != nil {
		return err
	}
	servers, err := decodeEach(w.MCPServers, "mcpServers", decodeMCPServer)
	r.MCPServers = servers
	return err
}

// Result of session/fork: the new session with its models, modes and config options
type ForkSessionResponse struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitzero"`
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	SessionID     string                `json:"sessionId"`
}

// Fills the response from its JSON object, which must carry sessionId
func (r *ForkSessionResponse) UnmarshalJSON(data []byte) error {
	type plain ForkSessionResponse
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "ForkSessionResponse", "sessionId"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	r.ConfigOptions = options
	return err
}

// Params of session/resume: the session, the working directory and the MCP servers the client offers
type ResumeSessionRequest struct {
	Meta       *jsonutil.Object `json:"_meta,omitempty"`
	Cwd        string           `json:"cwd"`
	MCPServers []MCPServer      `json:"mcpServers,omitzero"`
	SessionID  string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry cwd and sessionId
func (r *ResumeSessionRequest) UnmarshalJSON(data []byte) error {
	type plain ResumeSessionRequest
	var w struct {
		*plain
		MCPServers []json.RawMessage `json:"mcpServers"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "ResumeSessionRequest", "cwd", "sessionId"); err != nil {
		return err
	}
	servers, err := decodeEach(w.MCPServers, "mcpServers", decodeMCPServer)
	r.MCPServers = servers
	return err
}

// Result of session/resume: the session's models, modes and config options
type ResumeSessionResponse struct {
	Meta          *jsonutil.Object      `json:"_meta,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitzero"`
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
}

// Fills the response from its JSON object
func (r *ResumeSessionResponse) UnmarshalJSON(data []byte) error {
	type plain ResumeSessionResponse
	var w struct {
		*plain
		ConfigOptions []json.RawMessage `json:"configOptions"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "ResumeSessionResponse"); err != nil {
		return err
	}
	options, err := decodeEach(w.ConfigOptions, "configOptions", decodeSessionConfigOption)
	r.ConfigOptions = options
	return err
}

// Params of session/close
type CloseSessionRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry sessionId
func (r *CloseSessionRequest) UnmarshalJSON(data []byte) error {
	type plain CloseSessionRequest
	return decodeObject(data, (*plain)(r), "CloseSessionRequest", "sessionId")
}

// Result of session/close
type CloseSessionResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Params of session/set_mode
type SetSessionModeRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	ModeID    string           `json:"modeId"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry modeId and sessionId
func (r *SetSessionModeRequest) UnmarshalJSON(data []byte) error {
	type plain SetSessionModeRequest
	return decodeObject(data, (*plain)(r), "SetSessionModeRequest", "modeId", "sessionId")
}

// Result of session/set_mode
type SetSessionModeResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Params of session/set_model
type SetSessionModelRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	ModelID   string           `json:"modelId"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry modelId and sessionId
func (r *SetSessionModelRequest) UnmarshalJSON(data []byte) error {
	type plain SetSessionModelRequest
	return decodeObject(data, (*plain)(r), "SetSessionModelRequest", "modelId", "sessionId")
}

// Result of session/set_model
type SetSessionModelResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
}

// Mode a session can run in
type SessionMode struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Description *string          `json:"description,omitempty"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
}

// Fills the mode from its JSON object, which must carry id and name
func (m *SessionMode) UnmarshalJSON(data []byte) error {
	type plain SessionMode
	return decodeObject(data, (*plain)(m), "SessionMode", "id", "name")
}

// Modes a session offers and the one it runs in
type SessionModeState struct {
	Meta           *jsonutil.Object `json:"_meta,omitempty"`
	AvailableModes []SessionMode    `json:"availableModes"`
	CurrentModeID  string           `json:"currentModeId"`
}

// Fills the state from its JSON object, which must carry availableModes and currentModeId
func (s *SessionModeState) UnmarshalJSON(data []byte) error {
	type plain SessionModeState
	return decodeObject(data, (*plain)(s), "SessionModeState", "availableModes", "currentModeId")
}

// Model a session can use
type ModelInfo struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Description *string          `json:"description,omitempty"`
	ModelID     string           `json:"modelId"`
	Name        string           `json:"name"`
}

// Fills the model from its JSON object, which must carry modelId and name
func (m *ModelInfo) UnmarshalJSON(data []byte) error {
	type plain ModelInfo
	return decodeObject(data, (*plain)(m), "ModelInfo", "modelId", "name")
}

// Models a session offers and the one it uses
type SessionModelState struct {
	Meta            *jsonutil.Object `json:"_meta,omitempty"`
	AvailableModels []ModelInfo      `json:"availableModels"`
	CurrentModelID  string           `json:"currentModelId"`
}

// Fills the state from its JSON object, which must carry availableModels and currentModelId
func (s *SessionModelState) UnmarshalJSON(data []byte) error {
	type plain SessionModelState
	return decodeObject(data, (*plain)(s), "SessionModelState", "availableModels", "currentModelId")
}

// Params of session/prompt: the user's message as content blocks
type PromptRequest struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	MessageID *string          `json:"messageId,omitempty"`
	Prompt    []ContentBlock   `json:"prompt"`
	SessionID string           `json:"sessionId"`
}

// Fills the request from its JSON object, which must carry prompt and sessionId
func (r *PromptRequest) UnmarshalJSON(data []byte) error {
	type plain PromptRequest
	var w struct {
		*plain
		Prompt []json.RawMessage `json:"prompt"`
	}
	w.plain = (*plain)(r)
	if err := decodeObject(data, &w, "PromptRequest", "prompt", "sessionId"); err != nil {
		return err
	}
	prompt, err := decodeEach(w.Prompt, "prompt", decodeContentBlock)
	r.Prompt = prompt
	return err
}

// Result of session/prompt: why the turn stopped and what it used
type PromptResponse struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	// end_turn, max_tokens, max_turn_requests, refusal or cancelled
	StopReason    string  `json:"stopReason"`
	Usage         *Usage  `json:"usage,omitempty"`
	UserMessageID *string `json:"userMessageId,omitempty"`
}

// Fills the response from its JSON object, which must carry stopReason
func (r *PromptResponse) UnmarshalJSON(data []byte) error {
	type plain PromptResponse
	return decodeObject(data, (*plain)(r), "PromptResponse", "stopReason")
}

// Tokens a prompt turn used
type Usage struct {
	CachedReadTokens  *int `json:"cachedReadTokens,omitempty"`
	CachedWriteTokens *int `json:"cachedWriteTokens,omitempty"`
	InputTokens       int  `json:"inputTokens"`
	OutputTokens      int  `json:"outputTokens"`
	ThoughtTokens     *int `json:"thoughtTokens,omitempty"`
	TotalTokens       int  `json:"totalTokens"`
}

// Fills the usage from its JSON object, which must carry inputTokens, outputTokens and totalTokens
func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	return decodeObject(data, (*plain)(u), "Usage", "inputTokens", "outputTokens", "totalTokens")
}

// Params of session/cancel
type CancelNotification struct {
	Meta      *jsonutil.Object `json:"_meta,omitempty"`
	SessionID string           `json:"sessionId"`
}

// Fills the notification from its JSON object, which must carry sessionId
func (n *CancelNotification) UnmarshalJSON(data []byte) error {
	type plain CancelNotification
	return decodeObject(data, (*plain)(n), "CancelNotification", "sessionId")
}
