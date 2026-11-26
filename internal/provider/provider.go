package provider

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

type APIMode string

const (
	ModeChatCompletions   APIMode = "chat_completions"
	ModeAnthropicMessages APIMode = "anthropic_messages"
	ModeCodexResponses    APIMode = "codex_responses"
	ModeGeminiNative      APIMode = "gemini_native"
	ModeBedrockConverse   APIMode = "bedrock_converse"
)

type AuthKind string

const (
	AuthNone     AuthKind = "none"
	AuthAPIKey   AuthKind = "api_key"
	AuthOAuth    AuthKind = "oauth"
	AuthAWSSigV4 AuthKind = "aws_sigv4"
	AuthGCPOAuth AuthKind = "gcp_oauth"
)

const (
	AuthTypeAPIKey          = "api_key"
	AuthTypeOAuthDeviceCode = "oauth_device_code"
	AuthTypeOAuthExternal   = "oauth_external"
	AuthTypeOAuthMiniMax    = "oauth_minimax"
	AuthTypeExternalProcess = "external_process"
	AuthTypeAWSSDK          = "aws_sdk"
	AuthTypeVertex          = "vertex"
	AuthTypeVirtual         = "virtual"
)

func AuthKindOf(authType string) AuthKind {
	switch authType {
	case AuthTypeOAuthDeviceCode, AuthTypeOAuthExternal, AuthTypeOAuthMiniMax, AuthTypeExternalProcess:
		return AuthOAuth
	case AuthTypeAWSSDK:
		return AuthAWSSigV4
	case AuthTypeVertex:
		return AuthGCPOAuth
	case AuthTypeVirtual:
		return AuthNone
	}
	return AuthAPIKey
}

// Kind of provider a plugin registers: its API, how it signs in and its defaults. A profile has providers of these kinds, each with
// settings and accounts of its own
type Spec struct {
	Name        string
	DisplayName string
	Description string
	SignupURL   string
	// Base URL a provider of the kind starts with, and the variable that sets it for all of them
	BaseURL    string
	BaseURLEnv string
	ModelsURL  string
	Mode       APIMode
	Auth       AuthKind
	AuthType   string
	KeyEnv     []string
	Aliases    []string
	// Model a provider of the kind starts with
	DefaultModel               string
	FallbackModels             []string
	Headers                    map[string]string
	Local                      bool
	Aggregator                 bool
	Hidden                     bool
	ModelListing               bool
	HealthCheck                bool
	Vision                     bool
	VisionToolMessages         bool
	OmitTemperature            bool
	DefaultMaxTokens           int
	UnsupportedResponseFormats []string
	ModelsDevID                string
	Hostnames                  []string
	PerModelMode               func(model string) APIMode
	AmbientCredentials         bool

	// Settings each provider of the kind keeps besides its title, base URL, default model and models, keyed by their names below it
	Settings []config.Setting
	// Fields each account of a provider of the kind keeps, keyed by their names below it: an API key when none are named
	Account []config.Setting
	// Kind as one provider has it, from its settings once its base URL and default model are set, for a kind whose API or address
	// depends on more of them
	Resolve func(spec Spec, settings config.View) Spec

	SupportsStreaming          *bool
	SupportsPromptCacheKey     bool
	NativeReasoningDetailsType string
	ProcessCommand             string
	ProcessArgs                []string
	ProcessCommandEnvVars      []string
	ProcessArgsEnvVar          string
	ModelAliases               map[string]string
	Hostname                   string
	FixedTemperature           *float64
	DefaultAuxModel            string
	ModelCapabilities          map[string]map[string]any
	AccountCard                map[string]string
	MetadataHostnames          []string
	MessageCarrierOpaque       string
	MessageCarrierText         string
	ReplaysToolCallSignatures  bool
	StaticModels               []string
	CatalogCredentialFiles     []string
	PickerAfter                string
	PickerGroup                []string
	CatalogAliases             bool
	Overlay                    *IcebathOverlay
	OverlayAliases             []string
	RegistryAuthType           string
	RegistryAfter              string
	PortalBaseURL              string
	ClientID                   string
	Scope                      string
	Extra                      map[string]any

	ResponsesDropReplayItemIDs       bool
	ResponsesOmitPromptCacheKey      bool
	ResponsesIssuer                  string
	ResponsesDistrustCompletedStatus bool
	ResponsesNoNativeCompaction      bool
	ResponsesReasoningFromProfile    bool
}

type IcebathOverlay struct {
	Transport       string
	IsAggregator    bool
	AuthType        string
	ExtraEnvVars    []string
	BaseURLOverride string
	BaseURLEnvVar   string
}

func ModelsURL(base string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/models"
}

func BaseHost(base string) string {
	parsed, ok := parseBase(base)
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
}

func BasePath(base string) string {
	parsed, ok := parseBase(base)
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.ToLower(parsed.Path), "/")
}

func BaseHostMatches(base, domain string) bool {
	host := BaseHost(base)
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func parseBase(base string) (*url.URL, bool) {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return nil, false
	}
	scheme := strings.IndexFunc(raw, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-.", r)
	})
	if !strings.HasPrefix(raw, "//") && (scheme <= 0 || !strings.HasPrefix(raw[scheme:], "://")) {
		raw = "//" + raw
	}
	parsed, err := url.Parse(raw)
	return parsed, err == nil
}

func (s Spec) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

func (s Spec) KeyEnvNames() []string {
	out := make([]string, 0, len(s.KeyEnv))
	for _, v := range s.KeyEnv {
		if strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL") {
			continue
		}
		out = append(out, v)
	}
	return out
}

type Route struct {
	// Provider of the profile, by its name there
	Provider string
	// Kind of the provider, by the name of its spec
	Spec    string
	Model   string
	BaseURL string
	APIKey  string
	Headers map[string]string
	Mode    APIMode
	Auth    AuthKind
	Local   bool
}

type Reasoning struct {
	Effort string
}

type Request struct {
	Model     string
	Messages  []*sessionsv1.Message
	Tools     []map[string]any
	MaxTokens int
	Reasoning Reasoning
	SessionID string
}

type Response struct {
	Message      *sessionsv1.Message
	FinishReason string
}

type EventKind string

const (
	EventTextDelta      EventKind = "text_delta"
	EventReasoningDelta EventKind = "reasoning_delta"
)

type StreamEvent struct {
	Kind EventKind
	Text string
}

type StreamFunc func(StreamEvent) error

type Client interface {
	Route() Route
	Stream(ctx context.Context, request *Request, fn StreamFunc) (*Response, error)
}

type ErrorKind string

const (
	ErrAuth            ErrorKind = "auth"
	ErrRateLimit       ErrorKind = "rate_limit"
	ErrContextOverflow ErrorKind = "context_overflow"
	ErrInvalidRequest  ErrorKind = "invalid_request"
	ErrModelNotFound   ErrorKind = "model_not_found"
	ErrServer          ErrorKind = "server"
	ErrNetwork         ErrorKind = "network"
	ErrContentFilter   ErrorKind = "content_filter"
	ErrEmptyResponse   ErrorKind = "empty_response"
	ErrUnknown         ErrorKind = "unknown"
)

type APIError struct {
	Provider   string
	Model      string
	StatusCode int
	Message    string
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

func (e *APIError) Unwrap() error {
	return e.Err
}

func (e *APIError) Retryable() bool {
	switch e.Kind {
	case ErrRateLimit, ErrServer, ErrNetwork, ErrEmptyResponse:
		return true
	}
	return false
}

func KindOf(status int) ErrorKind {
	switch {
	case status == 401 || status == 403:
		return ErrAuth
	case status == 404:
		return ErrModelNotFound
	case status == 413:
		return ErrContextOverflow
	case status == 429:
		return ErrRateLimit
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrInvalidRequest
	}
	return ErrUnknown
}

type Factory func(route Route) (Client, error)

type Registrant interface {
	Specs() []Spec
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Specs() []Spec {
	var specs []Spec
	for _, registrant := range r.registrants {
		specs = append(specs, registrant.Specs()...)
	}
	return specs
}

// Spec of the kind with the name, or with it as an alias
func (r *Registry) Spec(name string) (Spec, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	specs := r.Specs()
	for _, spec := range specs {
		if spec.Name == key {
			return spec, true
		}
	}
	for _, spec := range specs {
		if slices.Contains(spec.Aliases, key) {
			return spec, true
		}
	}
	return Spec{}, false
}

type Provider struct {
	Registry
}

func New(cfg *config.Config) *Provider {
	p := &Provider{}
	providerconfig.Register(cfg, sections{registry: &p.Registry})
	return p
}
