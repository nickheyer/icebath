package mcp

import (
	"encoding/json"
	"strings"
)

// Protocol revisions this client negotiates, newest handshake revision last
var SupportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

// Newest revision the client knows, reached through the stateless server/discover probe
const LatestProtocolVersion = "2026-07-28"

// Newest revision the initialize handshake speaks and the MCP-Protocol-Version header is seeded from
const LatestHandshakeVersion = "2025-11-25"

// Protocol version the content type preflight POST offers
const probeProtocolVersion = "2025-03-26"

// ClientName is the clientInfo name every server sees
const ClientName = "icebath"

// ClientVersion is the clientInfo version, set by the binary at startup
var ClientVersion = "dev"

// Implementation names one side of the connection
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// ClientCapabilities advertised in initialize
type ClientCapabilities struct {
	Sampling     map[string]any `json:"sampling,omitzero"`
	Elicitation  map[string]any `json:"elicitation,omitzero"`
	Roots        map[string]any `json:"roots,omitempty"`
	Experimental map[string]any `json:"experimental,omitempty"`
}

// InitializeParams is the initialize request body
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// ServerCapabilities is the capabilities object of InitializeResult, nil members mean unsupported
type ServerCapabilities struct {
	Tools        map[string]any `json:"tools,omitempty"`
	Resources    map[string]any `json:"resources,omitempty"`
	Prompts      map[string]any `json:"prompts,omitempty"`
	Logging      map[string]any `json:"logging,omitempty"`
	Completions  map[string]any `json:"completions,omitempty"`
	Experimental map[string]any `json:"experimental,omitempty"`
}

// Has reports whether the named capability family is advertised
func (c *ServerCapabilities) Has(family string) bool {
	if c == nil {
		return false
	}
	switch family {
	case "tools":
		return c.Tools != nil
	case "resources":
		return c.Resources != nil
	case "prompts":
		return c.Prompts != nil
	case "logging":
		return c.Logging != nil
	case "completions":
		return c.Completions != nil
	}
	return false
}

// InitializeResult is the handshake answer, also the shape server/discover returns
type InitializeResult struct {
	ProtocolVersion string              `json:"protocolVersion"`
	Capabilities    *ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation      `json:"serverInfo"`
	Instructions    string              `json:"instructions,omitempty"`
}

// ToolAnnotations are the behavioural hints a server attaches to a tool
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is one entry of tools/list
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Meta         map[string]any   `json:"_meta,omitempty"`
}

// ReadOnly reports an exact true readOnlyHint, anything else counts as write capable
func (t *Tool) ReadOnly() bool {
	return t != nil && t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// listPage is the common shape of every paginated list result
type listPage struct {
	Tools      []*Tool         `json:"tools,omitempty"`
	Resources  []*Resource     `json:"resources,omitempty"`
	Prompts    []*Prompt       `json:"prompts,omitempty"`
	NextCursor string          `json:"nextCursor,omitempty"`
	TTLMs      *float64        `json:"ttlMs,omitempty"`
	CacheScope string          `json:"cacheScope,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// ContentBlock is one element of a tool result or prompt message content list
type ContentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Data        string            `json:"data,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Size        *int64            `json:"size,omitempty"`
	Resource    *ResourceContents `json:"resource,omitempty"`
	Meta        map[string]any    `json:"_meta,omitempty"`
}

// ResourceContents is the payload of an embedded resource or a resources/read entry
type ResourceContents struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType,omitempty"`
	Text     *string `json:"text,omitempty"`
	Blob     *string `json:"blob,omitempty"`
}

// CallToolResult is the answer to tools/call
type CallToolResult struct {
	Content           []*ContentBlock `json:"content"`
	StructuredContent any             `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              map[string]any  `json:"_meta,omitempty"`
}

// Resource is one entry of resources/list
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int64 `json:"size,omitempty"`
}

// ReadResourceResult is the answer to resources/read
type ReadResourceResult struct {
	Contents []*ResourceContents `json:"contents"`
}

// PromptArgument describes one prompt parameter
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    *bool  `json:"required,omitempty"`
}

// Prompt is one entry of prompts/list
type Prompt struct {
	Name        string            `json:"name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Arguments   []*PromptArgument `json:"arguments,omitempty"`
}

// PromptMessage is one message of prompts/get
type PromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// GetPromptResult is the answer to prompts/get
type GetPromptResult struct {
	Description string           `json:"description,omitempty"`
	Messages    []*PromptMessage `json:"messages"`
}

// LoggingMessageParams is notifications/message
type LoggingMessageParams struct {
	Level  string          `json:"level"`
	Logger string          `json:"logger,omitempty"`
	Data   json.RawMessage `json:"data"`
}

// ProgressParams is notifications/progress
type ProgressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// ModelHint names a preferred model in a sampling request
type ModelHint struct {
	Name string `json:"name,omitempty"`
}

// ModelPreferences carries the server's model wishes in a sampling request
type ModelPreferences struct {
	Hints                []ModelHint `json:"hints,omitempty"`
	CostPriority         *float64    `json:"costPriority,omitempty"`
	SpeedPriority        *float64    `json:"speedPriority,omitempty"`
	IntelligencePriority *float64    `json:"intelligencePriority,omitempty"`
}

// SamplingMessage is one conversation message of sampling/createMessage
type SamplingMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// SamplingTool is one tool the server offers the sampled model
type SamplingTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CreateMessageParams is sampling/createMessage
type CreateMessageParams struct {
	Messages         []SamplingMessage `json:"messages"`
	ModelPreferences *ModelPreferences `json:"modelPreferences,omitempty"`
	SystemPrompt     string            `json:"systemPrompt,omitempty"`
	IncludeContext   string            `json:"includeContext,omitempty"`
	Temperature      *float64          `json:"temperature,omitempty"`
	MaxTokens        int               `json:"maxTokens"`
	StopSequences    []string          `json:"stopSequences,omitempty"`
	Metadata         map[string]any    `json:"metadata,omitempty"`
	Tools            []SamplingTool    `json:"tools,omitempty"`
	ToolChoice       map[string]any    `json:"toolChoice,omitempty"`
}

// ElicitParams is elicitation/create
type ElicitParams struct {
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	URL             string         `json:"url,omitempty"`
	ElicitationID   string         `json:"elicitationId,omitempty"`
	// Raw requestedSchema bytes so the consent summary follows the server's property order
	RawRequestedSchema json.RawMessage `json:"-"`
}

// Decodes the params and keeps the requestedSchema bytes as sent
func (p *ElicitParams) UnmarshalJSON(data []byte) error {
	type plain ElicitParams
	var raw struct {
		plain
		RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = ElicitParams(raw.plain)
	if len(raw.RequestedSchema) > 0 && string(raw.RequestedSchema) != "null" {
		p.RawRequestedSchema = raw.RequestedSchema
		if err := json.Unmarshal(raw.RequestedSchema, &p.RequestedSchema); err != nil {
			return err
		}
	}
	return nil
}

// ElicitResult is the answer to elicitation/create
type ElicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitzero"`
}

// Method names used by this client
const (
	methodInitialize            = "initialize"
	methodDiscover              = "server/discover"
	methodInitialized           = "notifications/initialized"
	methodPing                  = "ping"
	methodToolsList             = "tools/list"
	methodToolsCall             = "tools/call"
	methodResourcesList         = "resources/list"
	methodResourcesRead         = "resources/read"
	methodPromptsList           = "prompts/list"
	methodPromptsGet            = "prompts/get"
	methodToolsListChanged      = "notifications/tools/list_changed"
	methodPromptsListChanged    = "notifications/prompts/list_changed"
	methodResourcesListChanged  = "notifications/resources/list_changed"
	methodLoggingMessage        = "notifications/message"
	methodProgress              = "notifications/progress"
	methodCancelled             = "notifications/cancelled"
	methodSamplingCreateMessage = "sampling/createMessage"
	methodElicitationCreate     = "elicitation/create"
	methodRootsList             = "roots/list"
)

// MCP logging levels in RFC 5424 order mapped onto slog levels
var logLevelMap = map[string]string{
	"debug": "debug", "info": "info", "notice": "info", "warning": "warn", "error": "error",
	"critical": "error", "alert": "error", "emergency": "error",
}

// supportedVersion reports whether the server named a handshake revision this client speaks
func supportedVersion(v string) bool {
	for _, s := range SupportedProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// baseMime lowers a MIME string and drops its parameters
func baseMime(mime string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
}
