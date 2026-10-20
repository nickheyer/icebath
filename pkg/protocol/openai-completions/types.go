package openaicompletions

import "encoding/json"

type request struct {
	Model               string         `json:"model"`
	Messages            []message      `json:"messages"`
	Tools               []tool         `json:"tools,omitempty"`
	ToolChoice          any            `json:"tool_choice,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Request message whose content is a string, a part list, or absent beside tool calls
type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type namedToolChoice struct {
	Type     string        `json:"type"`
	Function namedFunction `json:"function"`
}

type namedFunction struct {
	Name string `json:"name"`
}

type toolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function toolCallFunction `json:"function"`
}

// Arguments travel as a JSON encoded string rather than an object
type toolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type response struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage"`
}

type choice struct {
	Index        int             `json:"index"`
	Message      responseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type responseMessage struct {
	Content   *string    `json:"content"`
	Refusal   *string    `json:"refusal"`
	ToolCalls []toolCall `json:"tool_calls"`
}

type usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details"`
}

type promptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Streamed frame, either a completion chunk or an error object
type chunk struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *usage         `json:"usage"`
	Error   *apiError      `json:"error"`
}

type streamChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Content   *string         `json:"content"`
	Refusal   *string         `json:"refusal"`
	ToolCalls []toolCallChunk `json:"tool_calls"`
}

// Fragment of one tool call, keyed by index with every other field optional
type toolCallChunk struct {
	Index    int               `json:"index"`
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function *toolCallFunction `json:"function"`
}

type errorResponse struct {
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}
