// Neutral conversation, request, response and stream types every model wire protocol converts to and from
package wire

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Dialect a route speaks, in the word config and session rows persist
type Mode string

const (
	ModeChat           Mode = "chat_completions"
	ModeResponses      Mode = "codex_responses"
	ModeAnthropic      Mode = "anthropic_messages"
	ModeBedrock        Mode = "bedrock_converse"
	ModeCodexAppServer Mode = "codex_app_server"
)

// Author of a conversation message
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Function or provider-executed tool a request offers the model
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Builtin     string
	Cache       *CacheMarker
}

// Ephemeral prompt cache breakpoint, an empty TTL sending none
type CacheMarker struct {
	TTL string
}

// Which tool the model must or may call
type ToolChoice struct {
	Mode string
	Name string
}

// Thinking ask before a route clamps it
type Reasoning struct {
	Enabled bool
	Effort  string
	Payload json.RawMessage
}

// Structured output request
type ResponseFormat struct {
	Type   string
	Name   string
	Schema json.RawMessage
	Strict *bool
}

// Upstream provider preferences of an aggregator route
type ProviderRouting struct {
	Sort              string
	Only              []string
	Ignore            []string
	Order             []string
	RequireParameters *bool
	DataCollection    string
	ZeroRetention     bool
}

// Body field the output cap travels in
type MaxTokensField uint8

const (
	MaxTokens MaxTokensField = iota + 1
	MaxCompletionTokens
	MaxOutputTokens
)

// One provider request in neutral form with the per-protocol knobs the route policy decided
type Request struct {
	Model             string
	Messages          []Message
	Tools             []Tool
	ToolChoice        ToolChoice
	ParallelToolCalls *bool
	MaxTokens         int
	MaxTokensField    MaxTokensField
	Temperature       *float64
	OmitTemperature   bool
	TopP              *float64
	TopK              *int
	Stop              []string
	Reasoning         *Reasoning
	ServiceTier       string
	Speed             string
	Verbosity         string
	ResponseFormat    *ResponseFormat
	Instructions      string
	Include           []string
	Store             *bool
	PromptCacheKey    string
	CacheRetention    string
	ContextManagement json.RawMessage
	Routing           *ProviderRouting
	Metadata          json.RawMessage
	SessionID         string
	CacheScope        string
	ExtraBody         map[string]json.RawMessage
	TopLevel          map[string]json.RawMessage
	ExtraHeaders      map[string]string
	Stream            bool
	IncludeUsage      bool
	Timeout           time.Duration
	Chat              ChatOptions
	Responses         ResponsesOptions
	Anthropic         AnthropicOptions
	Gemini            GeminiOptions
	Bedrock           BedrockOptions
}

// Chat Completions replay and schema knobs
type ChatOptions struct {
	DeveloperRole     bool
	KeepSignatures    bool
	ReplayReasoning   ReasoningKeys
	NativeDetailsType string
	MistralToolIDs    bool
	AliasToolSearch   bool
	MoonshotSchemas   bool
	CacheKey          bool
	Carriers          CarrierKeys
}

// Set of reasoning keys an assistant message may carry on a route
type ReasoningKeys uint8

const (
	KeyReasoningContent ReasoningKeys = 1 << iota
	KeyReasoning
	KeyReasoningDetails
)

// Backend family a Responses endpoint belongs to
type ResponsesBackend uint8

const (
	ResponsesGeneric ResponsesBackend = iota
	ResponsesOpenAI
	ResponsesCodex
	ResponsesXAI
	ResponsesAzure
	ResponsesAzureFoundry
	ResponsesOpenCode
	ResponsesPerplexity
	ResponsesMeta
	ResponsesMantle
)

// Responses endpoint quirks and replay policy
type ResponsesOptions struct {
	Backend          ResponsesBackend
	ReplayItemIDs    bool
	CacheKeyField    bool
	TrustsCompleted  bool
	NativeCompaction bool
	ProfileReasoning bool
	IssuerKind       string
	IssuerModel      string
	ReplayEncrypted  bool
	NewestOnly       bool
	Efforts          []string
	AllTurns         bool
	NativeWebSearch  bool
	SanitizeHarmony  bool
	Identity         string
	NoContinuation   bool
	Prune            InputPruner
	RasterizeSVG     func(dataURL string) (png string, ok bool)
}

// Restructures converted Responses input around a replayed compaction checkpoint, sources parallel to items
type InputPruner func(items []json.RawMessage, sources []*Message) []json.RawMessage

// Anthropic Messages endpoint knobs
type AnthropicOptions struct {
	OAuth             bool
	PreserveDots      bool
	KeepCatalogIDs    bool
	ContextLength     int
	FastMode          bool
	DropContext1MBeta bool
	InvokeModel       bool
	BaseURL           string
	BufferToolInput   bool
	RasterizeSVG      func(dataURL string) (png string, ok bool)
}

// Gemini generateContent knobs
type GeminiOptions struct {
	GenerateMethod string
	StreamMethod   string
	Bearer         bool
	KeyGuidance    bool
	LongDecode     bool
	BaseURL        string
	KeyKind        string
}

// Bedrock Converse knobs
type BedrockOptions struct {
	Region            string
	Guardrail         *Guardrail
	CachePoints       bool
	RedactedReasoning bool
	ResolveProfile    func(arn string) string
}

// Bedrock guardrail binding
type Guardrail struct {
	ID               string
	Version          string
	Trace            string
	StreamProcessing string
}

// Builds the provider HTTP request for one neutral request
type Protocol interface {
	Build(req *Request) (*Call, error)
}

// One prepared provider request, its path joined to the endpoint base URL
type Call struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
	Stream bool
	Decode Decoder
}

// Reads one call's reply with the state its build recorded, a failed stream returning what arrived beside its error
type Decoder interface {
	Stream(ctx context.Context, body io.Reader, sink Sink) (*Response, error)
	Response(header http.Header, body []byte) (*Response, error)
	Error(status int, header http.Header, body []byte) error
}

// Where a call goes and how it authenticates
type Endpoint struct {
	BaseURL string
	Auth    Authorizer
	Header  http.Header
	Client  *http.Client
}

// Puts credentials on an outgoing request, minting or signing as needed
type Authorizer interface {
	Authorize(ctx context.Context, r *http.Request, body []byte) error
}

// Provider reached without HTTP, such as an agent program or a virtual provider
type Caller interface {
	Call(ctx context.Context, req *Request, sink Sink) (*Response, error)
}
