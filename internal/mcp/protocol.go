package mcp

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// Revisions the initialize handshake negotiates, newest last
var HandshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

const (
	// Newest revision this client knows, reached through the stateless server/discover probe
	LatestVersion = "2026-07-28"
	// Newest revision the handshake offers, and the MCP-Protocol-Version header a connection starts with
	HandshakeVersion = "2025-11-25"
	// Revision the content type preflight of a remote server offers
	ProbeVersion = "2025-03-26"
)

// clientInfo name every server sees
const ClientName = "icebath-agent"

// One side of a connection
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

type ClientCapabilities struct {
	Sampling     map[string]any `json:"sampling,omitzero"`
	Elicitation  map[string]any `json:"elicitation,omitzero"`
	Roots        map[string]any `json:"roots,omitempty"`
	Experimental map[string]any `json:"experimental,omitempty"`
}

type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// Capabilities a server advertises, a nil member meaning it lacks that family
type ServerCapabilities struct {
	Tools        map[string]any `json:"tools,omitempty"`
	Resources    map[string]any `json:"resources,omitempty"`
	Prompts      map[string]any `json:"prompts,omitempty"`
	Logging      map[string]any `json:"logging,omitempty"`
	Completions  map[string]any `json:"completions,omitempty"`
	Experimental map[string]any `json:"experimental,omitempty"`
}

// Whether the family, tools, resources, prompts, logging or completions, is advertised
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

// Answer of the handshake, the same shape server/discover returns
type InitializeResult struct {
	ProtocolVersion string              `json:"protocolVersion"`
	Capabilities    *ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation      `json:"serverInfo"`
	Instructions    string              `json:"instructions,omitempty"`
}

// Hints a server attaches to a tool about how it behaves
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// One entry of tools/list
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Meta         map[string]any   `json:"_meta,omitempty"`
}

// Whether the server declares the tool read only, an exact true hint; anything else may write
func (t *Tool) ReadOnly() bool {
	return t != nil && t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// Name shown to people: the tool's title, else its annotations' title, else its name
func (t *Tool) Label() string {
	switch {
	case t.Title != "":
		return t.Title
	case t.Annotations != nil && t.Annotations.Title != "":
		return t.Annotations.Title
	}
	return t.Name
}

// Shape every paginated list answer shares
type listPage struct {
	Tools      []*Tool     `json:"tools,omitempty"`
	Resources  []*Resource `json:"resources,omitempty"`
	Prompts    []*Prompt   `json:"prompts,omitempty"`
	NextCursor string      `json:"nextCursor,omitempty"`
	TTLMs      *float64    `json:"ttlMs,omitempty"`
	CacheScope string      `json:"cacheScope,omitempty"`
}

// One element of the content of a tool result or of a prompt message
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

// Payload of an embedded resource or of one resources/read entry
type ResourceContents struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType,omitempty"`
	Text     *string `json:"text,omitempty"`
	Blob     *string `json:"blob,omitempty"`
}

// Answer of tools/call
type CallToolResult struct {
	Content           []*ContentBlock `json:"content"`
	StructuredContent any             `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Meta              map[string]any  `json:"_meta,omitempty"`
}

// Decodes structured content and _meta with numbers kept exact
func (r *CallToolResult) UnmarshalJSON(data []byte) error {
	var wire struct {
		Content           []*ContentBlock `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
		Meta              json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = CallToolResult{Content: wire.Content, IsError: wire.IsError}
	if len(wire.StructuredContent) > 0 && string(wire.StructuredContent) != "null" {
		value, err := DecodeExact(wire.StructuredContent)
		if err != nil {
			return err
		}
		r.StructuredContent = value
	}
	if len(wire.Meta) > 0 && string(wire.Meta) != "null" {
		value, err := DecodeExact(wire.Meta)
		if err != nil {
			return err
		}
		meta, ok := value.(map[string]any)
		if !ok {
			return &json.UnmarshalTypeError{Value: "non-object", Field: "_meta"}
		}
		r.Meta = meta
	}
	return nil
}

// First text the result holds, empty when it has none
func (r *CallToolResult) Text() string {
	for _, block := range r.Content {
		if block != nil && block.Text != "" {
			return block.Text
		}
	}
	return ""
}

// JSON value decoded with numbers as json.Number, so big integers stay exact
func DecodeExact(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

// One entry of resources/list
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        *int64 `json:"size,omitempty"`
}

// Answer of resources/read
type ReadResourceResult struct {
	Contents []*ResourceContents `json:"contents"`
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    *bool  `json:"required,omitempty"`
}

// One entry of prompts/list
type Prompt struct {
	Name        string            `json:"name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Arguments   []*PromptArgument `json:"arguments,omitempty"`
}

type PromptMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Answer of prompts/get
type GetPromptResult struct {
	Description string           `json:"description,omitempty"`
	Messages    []*PromptMessage `json:"messages"`
}

// notifications/progress
type ProgressParams struct {
	ProgressToken json.RawMessage `json:"progressToken"`
	Progress      float64         `json:"progress"`
	Total         *float64        `json:"total,omitempty"`
	Message       string          `json:"message,omitempty"`
}

type ModelHint struct {
	Name string `json:"name,omitempty"`
}

// Models a server wishes a sampling request to run on
type ModelPreferences struct {
	Hints                []ModelHint `json:"hints,omitempty"`
	CostPriority         *float64    `json:"costPriority,omitempty"`
	SpeedPriority        *float64    `json:"speedPriority,omitempty"`
	IntelligencePriority *float64    `json:"intelligencePriority,omitempty"`
}

type SamplingMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool a server offers the model of a sampling request
type SamplingTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// sampling/createMessage
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

// elicitation/create
type ElicitParams struct {
	Message         string         `json:"message"`
	RequestedSchema map[string]any `json:"requestedSchema,omitempty"`
	Mode            string         `json:"mode,omitempty"`
	URL             string         `json:"url,omitempty"`
	ElicitationID   string         `json:"elicitationId,omitempty"`
	// Property names of the requested schema in the order the server wrote them
	Order []string `json:"-"`
}

// Decodes the params, keeping the order of the requested schema's properties
func (p *ElicitParams) UnmarshalJSON(data []byte) error {
	type plain ElicitParams
	var wire struct {
		plain
		RequestedSchema json.RawMessage `json:"requestedSchema"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*p = ElicitParams(wire.plain)
	if len(wire.RequestedSchema) == 0 || string(wire.RequestedSchema) == "null" {
		return nil
	}
	if err := json.Unmarshal(wire.RequestedSchema, &p.RequestedSchema); err != nil {
		return err
	}
	var schema struct {
		Properties json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(wire.RequestedSchema, &schema) == nil {
		p.Order = Keys(schema.Properties)
	}
	return nil
}

// Keys of a JSON object in the order they are written, none for anything else
func Keys(raw json.RawMessage) []string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return keys
		}
		key, _ := token.(string)
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
		var skip json.RawMessage
		if decoder.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}

// Answer of elicitation/create: accept, decline or cancel
type ElicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitzero"`
}

// Methods of the protocol
const (
	MethodInitialize           = "initialize"
	MethodDiscover             = "server/discover"
	MethodInitialized          = "notifications/initialized"
	MethodPing                 = "ping"
	MethodToolsList            = "tools/list"
	MethodToolsCall            = "tools/call"
	MethodResourcesList        = "resources/list"
	MethodResourcesRead        = "resources/read"
	MethodPromptsList          = "prompts/list"
	MethodPromptsGet           = "prompts/get"
	MethodToolsListChanged     = "notifications/tools/list_changed"
	MethodPromptsListChanged   = "notifications/prompts/list_changed"
	MethodResourcesListChanged = "notifications/resources/list_changed"
	MethodLoggingMessage       = "notifications/message"
	MethodProgress             = "notifications/progress"
	MethodCancelled            = "notifications/cancelled"
	MethodCreateMessage        = "sampling/createMessage"
	MethodElicit               = "elicitation/create"
	MethodElicitationComplete  = "notifications/elicitation/complete"
)

// Whether the server named a revision the handshake speaks
func handshakeVersion(v string) bool {
	return slices.Contains(HandshakeVersions, v)
}

// Whether a revision is known and at least as new as the minimum
func VersionAtLeast(version, minimum string) bool {
	known := append(slices.Clone(HandshakeVersions), LatestVersion)
	v, m := slices.Index(known, version), slices.Index(known, minimum)
	return v >= 0 && m >= 0 && v >= m
}

// MIME type lowered without its parameters
func BaseMime(mime string) string {
	base, _, _ := strings.Cut(mime, ";")
	return strings.ToLower(strings.TrimSpace(base))
}
