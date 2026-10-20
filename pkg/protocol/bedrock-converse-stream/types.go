package bedrockconversestream

import "encoding/json"

type request struct {
	Messages        []message        `json:"messages,omitempty"`
	System          []systemBlock    `json:"system,omitempty"`
	InferenceConfig *inferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig      `json:"toolConfig,omitempty"`
}

type message struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// Union with exactly one member set
type systemBlock struct {
	Text       *string     `json:"text,omitempty"`
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type cachePoint struct {
	Type string `json:"type"`
}

// Union with exactly one member set
type contentBlock struct {
	Text             *string           `json:"text,omitempty"`
	Image            *imageBlock       `json:"image,omitempty"`
	Document         *documentBlock    `json:"document,omitempty"`
	Video            *videoBlock       `json:"video,omitempty"`
	ToolUse          *toolUseBlock     `json:"toolUse,omitempty"`
	ToolResult       *toolResultBlock  `json:"toolResult,omitempty"`
	ReasoningContent *reasoningContent `json:"reasoningContent,omitempty"`
	CitationsContent *citationsContent `json:"citationsContent,omitempty"`
}

type imageBlock struct {
	Format string     `json:"format"`
	Source byteSource `json:"source"`
}

type documentBlock struct {
	Format string     `json:"format"`
	Name   string     `json:"name"`
	Source byteSource `json:"source"`
}

type videoBlock struct {
	Format string     `json:"format"`
	Source byteSource `json:"source"`
}

// Bytes serialize as base64 per the restJson1 blob rule
type byteSource struct {
	Bytes []byte `json:"bytes"`
}

type toolUseBlock struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []toolResultContent `json:"content"`
	Status    string              `json:"status,omitempty"`
}

type toolResultContent struct {
	Text     *string        `json:"text,omitempty"`
	Image    *imageBlock    `json:"image,omitempty"`
	Document *documentBlock `json:"document,omitempty"`
}

type reasoningContent struct {
	ReasoningText   *reasoningText `json:"reasoningText,omitempty"`
	RedactedContent string         `json:"redactedContent,omitempty"`
}

type reasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// Generated text with the citations backing it
type citationsContent struct {
	Content   []citationText `json:"content"`
	Citations []citation     `json:"citations"`
}

type citationText struct {
	Text string `json:"text"`
}

type citation struct {
	Title         string           `json:"title"`
	SourceContent []citationText   `json:"sourceContent"`
	Location      citationLocation `json:"location"`
}

// Union over the char, page and chunk location shapes
type citationLocation struct {
	DocumentChar  *documentSpan `json:"documentChar"`
	DocumentPage  *documentSpan `json:"documentPage"`
	DocumentChunk *documentSpan `json:"documentChunk"`
}

type documentSpan struct {
	DocumentIndex int `json:"documentIndex"`
	Start         int `json:"start"`
	End           int `json:"end"`
}

type inferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type toolConfig struct {
	Tools      []tool      `json:"tools"`
	ToolChoice *toolChoice `json:"toolChoice,omitempty"`
}

// Union with exactly one member set
type tool struct {
	ToolSpec   *toolSpec   `json:"toolSpec,omitempty"`
	CachePoint *cachePoint `json:"cachePoint,omitempty"`
}

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema toolInputSchema `json:"inputSchema"`
}

type toolInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type toolChoice struct {
	Auto *struct{}     `json:"auto,omitempty"`
	Any  *struct{}     `json:"any,omitempty"`
	Tool *specificTool `json:"tool,omitempty"`
}

type specificTool struct {
	Name string `json:"name"`
}

type response struct {
	Output     converseOutput `json:"output"`
	StopReason string         `json:"stopReason"`
	Usage      tokenUsage     `json:"usage"`
	Metrics    metrics        `json:"metrics"`
}

type converseOutput struct {
	Message message `json:"message"`
}

type tokenUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

type metrics struct {
	LatencyMs int64 `json:"latencyMs"`
}

type contentBlockStartEvent struct {
	ContentBlockIndex int               `json:"contentBlockIndex"`
	Start             contentBlockStart `json:"start"`
}

type contentBlockStart struct {
	ToolUse *toolUseStart `json:"toolUse"`
}

type toolUseStart struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
}

type contentBlockDeltaEvent struct {
	ContentBlockIndex int               `json:"contentBlockIndex"`
	Delta             contentBlockDelta `json:"delta"`
}

type contentBlockDelta struct {
	Text             *string         `json:"text"`
	ToolUse          *toolUseDelta   `json:"toolUse"`
	ReasoningContent *reasoningDelta `json:"reasoningContent"`
	Citation         *citation       `json:"citation"`
}

type toolUseDelta struct {
	Input string `json:"input"`
}

type reasoningDelta struct {
	Text            *string `json:"text"`
	Signature       *string `json:"signature"`
	RedactedContent string  `json:"redactedContent"`
}

type contentBlockStopEvent struct {
	ContentBlockIndex int `json:"contentBlockIndex"`
}

type messageStopEvent struct {
	StopReason string `json:"stopReason"`
}

type metadataEvent struct {
	Usage   tokenUsage `json:"usage"`
	Metrics metrics    `json:"metrics"`
}

type exceptionBody struct {
	Message string `json:"message"`
}

type errorBody struct {
	Type    string `json:"__type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
