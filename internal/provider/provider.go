package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

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

type Profile struct {
	Name                       string
	DisplayName                string
	Description                string
	SignupURL                  string
	BaseURL                    string
	BaseURLEnv                 string
	ModelsURL                  string
	Mode                       APIMode
	Auth                       AuthKind
	AuthType                   string
	KeyEnv                     []string
	Aliases                    []string
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

func (p Profile) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

func (p Profile) KeyEnvNames() []string {
	out := make([]string, 0, len(p.KeyEnv))
	for _, v := range p.KeyEnv {
		if strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL") {
			continue
		}
		out = append(out, v)
	}
	return out
}

type Route struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	Headers  map[string]string
	Mode     APIMode
	Auth     AuthKind
	Local    bool
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
	Profiles() []Profile
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Profiles() []Profile {
	var profiles []Profile
	for _, registrant := range r.registrants {
		profiles = append(profiles, registrant.Profiles()...)
	}
	return profiles
}

func (r *Registry) Profile(name string) (Profile, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	profiles := r.Profiles()
	for _, profile := range profiles {
		if profile.Name == key {
			return profile, true
		}
	}
	for _, profile := range profiles {
		if slices.Contains(profile.Aliases, key) {
			return profile, true
		}
	}
	return Profile{}, false
}

type Provider struct {
	Registry
}

func New() *Provider {
	return &Provider{}
}
