// Wire protocol abstraction shared by every agentic API, independent of any provider
package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"sort"
	"sync"
)

// Name of a wire protocol, matching the api ids used by pi-ai
type ID string

const (
	AnthropicMessages     ID = "anthropic-messages"
	OpenAICompletions     ID = "openai-completions"
	OpenAIResponses       ID = "openai-responses"
	GoogleGenerativeAI    ID = "google-generative-ai"
	BedrockConverseStream ID = "bedrock-converse-stream"
	MistralConversations  ID = "mistral-conversations"
	OllamaChat            ID = "ollama-chat"
)

// Encodes neutral requests to one wire format and decodes its responses back
type Protocol interface {
	ID() ID
	// Reports what the wire format can carry so callers adapt a request before encoding
	Capabilities() Capabilities
	// Builds the HTTP call for req, with every protocol level header but no auth
	Encode(req Request) (Call, error)
	// Parses a complete non streaming response body
	Decode(r io.Reader) (Response, error)
	// Parses a streaming response body into neutral events ending with a done event
	Stream(r io.Reader) iter.Seq2[Event, error]
	// Parses a non 2xx response body into an Error carrying the wire message
	DecodeError(status int, body []byte) *Error
}

// Features a wire format carries, false where the encoder drops or rejects the feature
type Capabilities struct {
	Thinking         bool
	RedactedThinking bool
	Images           bool
	ImagesInResults  bool
	Documents        bool
	ToolChoiceNone   bool
	ToolChoiceNamed  bool
	StopSequences    bool
	ServerTools      bool
	Citations        bool
	Caching          bool
}

// HTTP request parts a provider completes with a base URL and credentials
type Call struct {
	Method string
	// Path relative to the provider base URL, already containing any model segment
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// Failure reported by the remote API with its wire level type and message
type Error struct {
	Status  int
	Type    string
	Message string
	Body    []byte
	// Overload or throttling the wire reports by type rather than status
	Transient bool
	// Arrived inside a 2xx streaming body rather than as an HTTP status
	InStream bool
}

func (e *Error) Error() string {
	if e.Type == "" {
		return fmt.Sprintf("protocol: http %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("protocol: http %d %s: %s", e.Status, e.Type, e.Message)
}

// Protocol neutral turn request
type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []Tool
	// Zero value means the model decides
	ToolChoice    ToolChoice
	MaxTokens     int
	Temperature   *float64
	StopSequences []string
	Stream        bool
	// Raw JSON deep merged over the encoded body for protocol specific fields
	Extra map[string]json.RawMessage
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// Carries only ToolResult parts answering the previous assistant turn
	RoleTool Role = "tool"
)

// One conversation turn made of ordered parts
type Message struct {
	Role    Role
	Content []Part
}

// Marker implemented by Text, Image, Document, Thinking, ToolCall, ToolResult, ServerToolCall and ServerToolResult
type Part interface {
	isPart()
}

// Model or user prose with any source citations the protocol attached to it
type Text struct {
	Text      string
	Citations []Citation
}

// Source reference attached to a span of text
type Citation struct {
	Title string
	URL   string
	// Name or index of a supplied document the span was drawn from
	Document string
	// Character offsets of the span inside the owning text
	Start int
	End   int
	// Quoted source passage when the protocol supplies one
	Cited string
}

// Raw image bytes with their IANA media type
type Image struct {
	MediaType string
	Data      []byte
}

// File contents such as a PDF or plain text handed to the model as a document
type Document struct {
	MediaType string
	Data      []byte
	Name      string
}

// Model reasoning with the opaque replay token the protocol requires on the next turn
type Thinking struct {
	Text      string
	Signature string
	Redacted  bool
	// Protocol that issued the signature, so other protocols know not to replay it
	Origin ID
}

// Model request to run a tool, with arguments as a JSON object
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
	// Opaque replay token some protocols attach to the call itself
	Signature string
	// Protocol that issued the signature
	Origin ID
}

// Outcome of a tool run addressed to the call that asked for it
type ToolResult struct {
	CallID  string
	Name    string
	Content []Part
	IsError bool
}

// Tool the vendor ran on its own side, such as web search, kept verbatim for replay
type ServerToolCall struct {
	ID     string
	Name   string
	Args   json.RawMessage
	Origin ID
}

// Raw outcome of a vendor side tool, replayed only to the protocol that produced it
type ServerToolResult struct {
	CallID  string
	Name    string
	Content json.RawMessage
	Origin  ID
}

func (Text) isPart()             {}
func (Image) isPart()            {}
func (Document) isPart()         {}
func (Thinking) isPart()         {}
func (ToolCall) isPart()         {}
func (ToolResult) isPart()       {}
func (ServerToolCall) isPart()   {}
func (ServerToolResult) isPart() {}

// Reports whether a signed part may be replayed to protocol id, true when no origin was recorded
func Replayable(origin, id ID) bool {
	return origin == "" || origin == id
}

// Callable function offered to the model with a JSON Schema for its arguments
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool"
)

// Constraint on whether and which tool the model must call
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string
}

var (
	registryMu sync.RWMutex
	registry   = map[ID]Protocol{}
)

// Makes p available to Lookup under its ID, panicking on a duplicate
func Register(p Protocol) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[p.ID()]; dup {
		panic("protocol: duplicate registration of " + string(p.ID()))
	}
	registry[p.ID()] = p
}

// Finds a registered protocol by name
func Lookup(id ID) (Protocol, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[id]
	return p, ok
}

// Lists registered protocol names in sorted order
func IDs() []ID {
	registryMu.RLock()
	defer registryMu.RUnlock()
	ids := make([]ID, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
