package mistralconversations

import "encoding/json"

type request struct {
	Model          string          `json:"model"`
	Instructions   string          `json:"instructions,omitempty"`
	Inputs         []any           `json:"inputs"`
	Tools          []functionTool  `json:"tools,omitempty"`
	CompletionArgs *completionArgs `json:"completion_args,omitempty"`
	Stream         bool            `json:"stream"`
	Store          bool            `json:"store"`
}

type messageEntry struct {
	Object  string `json:"object"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type functionCallEntry struct {
	Object     string `json:"object"`
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
}

type functionResultEntry struct {
	Object     string `json:"object"`
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Result     string `json:"result"`
}

type textChunk struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageChunk struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
}

type thinkChunk struct {
	Type      string      `json:"type"`
	Thinking  []textChunk `json:"thinking"`
	Signature string      `json:"signature,omitempty"`
}

type functionTool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type completionArgs struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	ToolChoice  string   `json:"tool_choice,omitempty"`
}

type response struct {
	ConversationID string      `json:"conversation_id"`
	Outputs        []wireEntry `json:"outputs"`
	Usage          usage       `json:"usage"`
}

// Output entry of any type, with content and arguments kept raw for their two wire forms
type wireEntry struct {
	Type               string          `json:"type"`
	ID                 string          `json:"id"`
	Model              string          `json:"model"`
	Content            json.RawMessage `json:"content"`
	ToolCallID         string          `json:"tool_call_id"`
	Name               string          `json:"name"`
	Arguments          json.RawMessage `json:"arguments"`
	ConfirmationStatus json.RawMessage `json:"confirmation_status"`
	AgentID            json.RawMessage `json:"agent_id"`
	Info               json.RawMessage `json:"info"`
	PreviousAgentID    string          `json:"previous_agent_id"`
	PreviousAgentName  string          `json:"previous_agent_name"`
	NextAgentID        string          `json:"next_agent_id"`
	NextAgentName      string          `json:"next_agent_name"`
}

type wireChunk struct {
	Type      string      `json:"type"`
	Text      string      `json:"text"`
	Thinking  []wireChunk `json:"thinking"`
	Signature string      `json:"signature"`
	Tool      string      `json:"tool"`
	Title     string      `json:"title"`
	URL       string      `json:"url"`
	FileID    string      `json:"file_id"`
	FileName  string      `json:"file_name"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Data payload of any streaming event
type event struct {
	Type               string          `json:"type"`
	ConversationID     string          `json:"conversation_id"`
	ID                 string          `json:"id"`
	ContentIndex       int             `json:"content_index"`
	Model              string          `json:"model"`
	Content            json.RawMessage `json:"content"`
	Name               string          `json:"name"`
	ToolCallID         string          `json:"tool_call_id"`
	Arguments          string          `json:"arguments"`
	ConfirmationStatus json.RawMessage `json:"confirmation_status"`
	AgentID            json.RawMessage `json:"agent_id"`
	Info               json.RawMessage `json:"info"`
	PreviousAgentID    string          `json:"previous_agent_id"`
	PreviousAgentName  string          `json:"previous_agent_name"`
	NextAgentID        string          `json:"next_agent_id"`
	NextAgentName      string          `json:"next_agent_name"`
	Usage              *usage          `json:"usage"`
	Message            string          `json:"message"`
	Code               int             `json:"code"`
}

// Error body of the object error shape, code arriving as a number or a string
type errorObject struct {
	Object  string          `json:"object"`
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

// Error body of the detail shape, a plain string or a validation list
type detailError struct {
	Detail json.RawMessage `json:"detail"`
}

type validationItem struct {
	Loc  []any  `json:"loc"`
	Msg  string `json:"msg"`
	Type string `json:"type"`
}
