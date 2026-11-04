// Package provider resolves model routes and speaks every supported inference wire protocol
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Wire protocol a route speaks
type APIMode string

const (
	ModeChatCompletions   APIMode = "chat_completions"
	ModeAnthropicMessages APIMode = "anthropic_messages"
	ModeCodexResponses    APIMode = "codex_responses"
	ModeGeminiNative      APIMode = "gemini_native"
)

// Credential style a route authenticates with
type AuthKind string

const (
	AuthNone     AuthKind = "none"
	AuthAPIKey   AuthKind = "api_key"
	AuthOAuth    AuthKind = "oauth"
	AuthAWSSigV4 AuthKind = "aws_sigv4"
	AuthGCPOAuth AuthKind = "gcp_oauth"
)

// Route is one fully resolved way to reach a model
type Route struct {
	Provider   string
	Model      string
	BaseURL    string
	APIKey     string
	Headers    map[string]string
	Mode       APIMode
	Auth       AuthKind
	Region     string
	ProjectID  string
	Deployment string
	APIVersion string
	Timeout    time.Duration
	Local      bool
	ExtraBody  map[string]any
	// AWS credentials for sigv4 signed routes
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Refreshes an expiring bearer token and returns the new one
	TokenSource func(ctx context.Context) (string, error)
	Transport   http.RoundTripper
}

// Reasoning controls thinking budget and effort for reasoning models
type Reasoning struct {
	Effort            string
	BudgetTokens      int
	Enabled           *bool
	Summary           string
	IncludeInResponse bool
}

// Request is one inference call in provider neutral shape with the system prompt as the first system message
type Request struct {
	Model             string
	Messages          []*state.Message
	Tools             []map[string]any
	ToolChoice        any
	ParallelToolCalls *bool
	MaxTokens         int
	Temperature       *float64
	TopP              *float64
	Stop              []string
	Reasoning         Reasoning
	ResponseFormat    map[string]any
	ExtraBody         map[string]any
	Stream            bool
	Timeout           time.Duration
	SessionID         string
	TurnID            string
	CacheTTL          string
	CacheScope        string
	ServiceTier       string
	Metadata          map[string]string
	User              string
}

// Usage counts tokens for one call
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	TotalTokens      int
}

// Add sums another usage into this one
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.TotalTokens += o.TotalTokens
}

// Response is one completed inference result
type Response struct {
	Message      *state.Message
	FinishReason string
	Usage        Usage
	Model        string
	Provider     string
	RequestID    string
	StatusCode   int
	Headers      http.Header
	Raw          json.RawMessage
	Latency      time.Duration
	Truncated    bool
}

// Kind of incremental stream event
type EventKind string

const (
	EventTextDelta      EventKind = "text_delta"
	EventReasoningDelta EventKind = "reasoning_delta"
	EventToolCallStart  EventKind = "tool_call_start"
	EventToolCallDelta  EventKind = "tool_call_delta"
	EventToolCallDone   EventKind = "tool_call_done"
	EventUsage          EventKind = "usage"
	EventStatus         EventKind = "status"
	EventDone           EventKind = "done"
)

// StreamEvent is one incremental piece of a streamed response
type StreamEvent struct {
	Kind     EventKind
	Text     string
	Index    int
	ToolCall *state.ToolCall
	Usage    *Usage
	Raw      json.RawMessage
}

// StreamFunc receives stream events and aborts the stream by returning an error
type StreamFunc func(StreamEvent) error

// Client speaks one wire protocol for one route
type Client interface {
	Route() Route
	Complete(ctx context.Context, req *Request) (*Response, error)
	Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error)
	CountTokens(ctx context.Context, req *Request) (int, error)
}

// Classified failure cause of an inference call
type ErrorKind string

const (
	ErrAuth            ErrorKind = "auth"
	ErrRateLimit       ErrorKind = "rate_limit"
	ErrQuota           ErrorKind = "quota"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrInvalidRequest  ErrorKind = "invalid_request"
	ErrModelNotFound   ErrorKind = "model_not_found"
	ErrServer          ErrorKind = "server"
	ErrNetwork         ErrorKind = "network"
	ErrTimeout         ErrorKind = "timeout"
	ErrContentFilter   ErrorKind = "content_filter"
	ErrEmptyResponse   ErrorKind = "empty_response"
	ErrCancelled       ErrorKind = "cancelled"
	ErrUnknown         ErrorKind = "unknown"
)

// APIError is a failed inference call classified for retry and fallback decisions
type APIError struct {
	Provider   string
	Model      string
	StatusCode int
	Code       string
	Type       string
	Message    string
	Body       string
	RequestID  string
	RetryAfter time.Duration
	Kind       ErrorKind
	Err        error
}

func (e *APIError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("%s/%s: %s (%d %s)", e.Provider, e.Model, e.Message, e.StatusCode, e.Kind)
	}
	return fmt.Sprintf("%s/%s: %s (%s)", e.Provider, e.Model, e.Message, e.Kind)
}

func (e *APIError) Unwrap() error { return e.Err }

// Retryable reports whether the same route may be tried again after a backoff
func (e *APIError) Retryable() bool {
	switch e.Kind {
	case ErrRateLimit, ErrServer, ErrNetwork, ErrTimeout, ErrEmptyResponse:
		return true
	}
	return false
}

// Adapter constructor for one wire protocol
type Factory func(route Route) (Client, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[APIMode]Factory{}
)

// RegisterFactory binds a wire protocol to its adapter constructor
func RegisterFactory(mode APIMode, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[mode] = f
}

// NewClient returns the adapter for the route's wire protocol
func NewClient(route Route) (Client, error) {
	factoriesMu.RLock()
	f, ok := factories[route.Mode]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: no adapter registered for api mode %q", route.Mode)
	}
	return f(route)
}
