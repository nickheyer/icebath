package anthropicmessages

import "encoding/json"

// Request body for creating a message
type request struct {
	Model         string      `json:"model"`
	MaxTokens     int         `json:"max_tokens"`
	System        string      `json:"system,omitempty"`
	Messages      []message   `json:"messages"`
	Tools         []tool      `json:"tools,omitempty"`
	ToolChoice    *toolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64    `json:"temperature,omitempty"`
	StopSequences []string    `json:"stop_sequences,omitempty"`
	Stream        bool        `json:"stream,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imageBlock struct {
	Type   string      `json:"type"`
	Source imageSource `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type thinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type redactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   []any  `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Response content block of any type, read by switching on Type
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	Data      string          `json:"data"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type outputTokensDetails struct {
	ThinkingTokens int `json:"thinking_tokens"`
}

type usage struct {
	InputTokens              int                  `json:"input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	OutputTokensDetails      *outputTokensDetails `json:"output_tokens_details"`
}

// Non streaming message body, also carried by message_start
type response struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      usage          `json:"usage"`
}

// Delta of a content block or of the message, read by switching on the event
type delta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	StopReason  string `json:"stop_reason"`
}

type streamEvent struct {
	Type         string        `json:"type"`
	Message      *response     `json:"message"`
	Index        int           `json:"index"`
	ContentBlock *contentBlock `json:"content_block"`
	Delta        *delta        `json:"delta"`
	Usage        *usage        `json:"usage"`
	Error        *errorObject  `json:"error"`
}

type errorObject struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorResponse struct {
	Type  string      `json:"type"`
	Error errorObject `json:"error"`
}
