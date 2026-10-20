package ollamachat

import "encoding/json"

type request struct {
	Model    string         `json:"model"`
	Messages []message      `json:"messages"`
	Tools    []tool         `json:"tools,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
	Stream   bool           `json:"stream"`
}

// Chat message for every role, images and tool calls riding beside the content string
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`
	Images     []string   `json:"images,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string           `json:"id,omitempty"`
	Function toolCallFunction `json:"function"`
}

// Arguments travel as a JSON object rather than a string
type toolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatResponse, one per NDJSON line when streaming, with the error line sharing the shape
type response struct {
	Model                 string  `json:"model"`
	Message               message `json:"message"`
	Done                  bool    `json:"done"`
	DoneReason            string  `json:"done_reason"`
	PromptEvalCount       int     `json:"prompt_eval_count"`
	PromptEvalCachedCount int     `json:"prompt_eval_cached_count"`
	EvalCount             int     `json:"eval_count"`
	Error                 string  `json:"error"`
}

type errorBody struct {
	Error string `json:"error"`
}
