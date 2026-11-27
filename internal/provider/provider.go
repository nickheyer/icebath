package provider

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
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
	// Place among the well-known kinds, which a person adding a provider is offered first, 1 at the top. A kind with none is listed by its
	// name with the rest
	Rank int
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
	// Route of a request once its credential and model are known, for a kind whose API or address depends on them, such as the coding
	// endpoint a Kimi coding key belongs to or the Bedrock API a model is served on; with the provider's settings, whose API setting, when
	// set, is the person's choice of API
	Route func(route Route, settings config.View) Route
	// Taken before any other kind when nothing names a provider and its credential is found, as an OpenRouter key is
	AutoFirst bool
	// Taken when nothing names a provider and no other kind has a credential, once its own is found outside icebath, as AWS's is
	AutoLast bool

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

	// The models.dev registry describes the kind's models better than its curated list, which then is only what the catalog offers when
	// the registry can't be read
	ModelsDevPreferred bool
	// Models it lists lead the catalog, the curated ones after them
	LiveFirst bool
	// Rows shaped as the models.dev registry's for models it has not indexed yet, by model id
	ModelMetadata map[string]map[string]any
	// Models whose ids are marked -vision take images though the registry doesn't know them
	VisionMarked bool
	// Its models listing states each model's context window, which is read before anything else knows it
	ListsContext bool
	// Its windows change with what it serves, so a window found is asked for again rather than kept
	ProbeAlways bool
	// Ids it still lists but no longer serves, left out of its catalog
	Delisted []string
	// Model a route of the kind lands on when none is picked and the online catalog names none
	CatalogDefault string
	// Context window the kind knows a model has on an endpoint, 0 when it doesn't; trusted before any probe
	ContextLength func(model, baseURL string) int
	// Published prices of the kind's models by id
	Prices map[string]Price
	// Prices of the requests a tier other than the standard one served, by the tier as the reply names it and then by model id
	TierPrices map[string]map[string]Price
	// Domain of a first party API billed per token at the list prices models.dev keeps, for requests sent to it over https
	ListPrices string
	// Requests are covered by a subscription rather than billed
	Included bool
	// Pages where credit is added, for the vendors the kind reaches
	Billing []Billing
	// Limits of the account a route of the kind signs in with: nil without an account to ask about
	AccountUsage func(ctx context.Context, route Route) (*AccountUsage, error)
	// Spends one banked reset of the account's limits, unless they aren't spent and force is false; what came of it
	ResetUsage func(ctx context.Context, route Route, force bool) string

	// What the kind changes in the requests sent to it, each hook called by the wire that sends them; a nil hook changes nothing

	// Messages as the wire sends them, rewritten for the kind, such as with fields its schema refuses taken out
	Messages func(call *Call, messages []map[string]any) []map[string]any
	// Reasoning a request that asks none is sent with; an ask that isn't set for none
	DefaultReasoning func(call *Call) Ask
	// Fields the kind sets for the request's reasoning and its own options: those added to the body after the request's own, and those put
	// over its top fields, where extra_headers holds headers of the request
	Shape func(call *Call) (extra, top map[string]any)
	// Fields added to the body of every request of the kind, such as its session or routing options
	ExtraBody func(call *Call) map[string]any
	// Output cap a request is sent with, given the one it asks for or 0; 0 sends none. Without the hook a request asking none takes the
	// kind's DefaultMaxTokens
	OutputCap func(call *Call, requested int) int
	// Field the output cap goes in at the address, empty for the wire's own choice
	OutputField func(baseURL, model string) string
	// Headers of one request, over the route's
	RequestHeaders func(call *Call) map[string]string
	// Efforts the kind's model takes, false when the kind declares none
	Efforts func(ctx context.Context, model string) ([]string, bool)
	// Reasoning field of a Responses request to the kind, nil to send none
	ReasoningPayload func(call *Call) map[string]any
	// Last changes the kind makes to the body of a Responses request, such as the speed tier its account offers
	Finish func(call *Call, body map[string]any)
	// Fields a reply carried beside its text that the kind wants back when the reply is replayed, of those kept under their names
	Replay func(call *Call, kept map[string]string) map[string]string
	// Whether tool calls are replayed with the thought signatures of their functions
	FunctionSignatures func(model, baseURL string) bool
	// Model id as the kind names it, such as with the vendor an aggregator wants in front; given the kind itself
	Native func(spec Spec, model string) string
	// Lines added to the error of a refusal at the address with the status and message, such as what the address can't serve
	RejectionHint func(status int, baseURL, message string) []string
	// Status line once the route's credential was renewed for the reason, unauthorized or stale; empty for the generic one
	RefreshNotice func(reason string) string
	// Reasoning fields of its replies it reads back: reasoning_content, reasoning, reasoning_details. Nil takes those of the kind claiming
	// the address, else the default; empty replays none
	Carriers []string
	// Refuses a tool call turn whose replayed assistant message lacks its reasoning_content
	EchoesReasoning bool
	// Tool names the kind keeps for its own tools, which the agent's tools of those names are sent under other names for
	ReservedTools []string
	// Takes only nine character tool call ids, as Mistral does
	ShortToolIDs bool
	// Model a side job on images runs on, for a kind whose helper model can't see; and whether none of the kind's models see images
	VisionHelper string
	Blind        bool
	// Whether requests for the kind's model over the API carry prompt cache markers, and whether on their blocks as the Anthropic API
	// places them; ok false leaves it to the wire
	Caching func(model string, mode APIMode) (on, native, ok bool)
	// Longest the kind keeps a prompt cache of the model when an hour is asked: 1h, or 5m for one that keeps it no longer
	CacheTTL func(model string) string
	// Its model ids keep their dots over the Anthropic API, where a Claude id's dots otherwise become dashes
	DottedModels bool
}

// Price of a model in USD for a million tokens, as decimal text; an empty field is not known
type Price struct {
	Input      string
	Output     string
	CacheRead  string
	CacheWrite string
	// Paid once for each request
	Request string
	// Tokens of prompt past which every token of the request takes the Above prices, 0 for none
	Threshold int
	Above     *Price
	// Where the prices are published, and which edition of them these are
	URL     string
	Version string
}

// Where a person adds credit for a vendor: its name, its billing page, and the hosts of its API when they differ from the kind's
type Billing struct {
	Label string
	URL   string
	Hosts []string
	// Page of the usage of a subscription, for accounts signed in with one
	Plan string
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

// Names of the efforts as a person reads them
var effortTitles = map[string]string{"none": "No reasoning", "minimal": "Minimal", "low": "Low", "medium": "Medium", "high": "High",
	"xhigh": "Extra high", "max": "Maximum"}

// Choices of a setting of the reasoning effort: the one given for no effort, then every effort
func EffortOptions(unset config.Option) []config.Option {
	options := []config.Option{unset}
	for _, effort := range Efforts {
		options = append(options, config.Option{Value: effort, Title: cmp.Or(effortTitles[effort], effort)})
	}
	return options
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
	// Account of the provider whose credential the route carries, empty for one from the environment or another program
	Account string
	// Signs a request whose provider takes a signature rather than a key, such as AWS SigV4; nil for every other route
	Sign func(request *http.Request, body []byte) error
	// Region the route's requests are signed for and served in, such as an AWS region; empty when its provider has none
	Region string
	// Keeps the route's credential as requests go out with it; nil when nothing keeps it
	Keeper *Keeper
}

// What keeps the credential of a route: told of each request sent with it, and asked for the route to send a refused one again with
type Keeper struct {
	Sent func()
	// Forgets the limit the route's account ran into, once the provider says it lifted, such as after a banked reset was spent
	Lifted func()
	// Route to send the refused request again with, its credential renewed or another account's; false when none would do better, which
	// leaves the failure to the caller's own retries and fallbacks
	Recover func(ctx context.Context, failure *APIError) (Route, bool)
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
	// Sampling temperature, the model's own when nil
	Temperature *float64
	// Shape the reply must take, free text when nil
	Format *Format
	// Fields put at the top of the request body as they are, over the ones the request sets, such as provider options a setting names
	Extra map[string]any
	// Start of the system prompt that stays the same from session to session, which a provider caching prompts in blocks caches on its
	// own; empty when nothing is known to stay
	StaticSystemPrefix string
	// Backend the web_search tool of the request searches with, such as openai-native or xai, which a provider's own server side search
	// stands in for; empty when the request has no web_search tool
	WebSearch string
	// Settings of the session the request is for, those of the profile in use when nil
	Config *config.Config
	// When the run the request is part of must end, which caps how long a silent stream is waited on; zero for no limit
	Deadline time.Time
	// First request of a turn the person started, rather than one the agent makes after tool calls
	UserTurn bool
	// Where the session comes from, such as cli, tui, cron, subagent or oneshot, which paces how long a prompt cache is worth keeping;
	// empty when not known
	Source string
	// Handed each failure of the request as the wire classified it, before the wire acts on it, so a plugin's classification may change
	// it; nil for none
	Classify func(failure *APIError)
}

// Reasons a plugin may classify a failed request with, and the kind of failure each counts as
var Reasons = map[string]ErrorKind{
	"auth": ErrAuth, "auth_permanent": ErrAuth, "billing": ErrQuota, "rate_limit": ErrRateLimit, "upstream_rate_limit": ErrRateLimit,
	"upstream_blocked": ErrServer, "overloaded": ErrServer, "server_error": ErrServer, "timeout": ErrNetwork, "ssl_cert_verification": ErrNetwork,
	"context_overflow": ErrContextOverflow, "payload_too_large": ErrContextOverflow, "image_too_large": ErrContextOverflow,
	"long_context_tier": ErrContextOverflow, "image_corrupt": ErrInvalidRequest, "model_not_found": ErrModelNotFound,
	"model_entitlement": ErrModelNotFound, "provider_policy_blocked": ErrContentFilter, "data_policy_denied": ErrContentFilter,
	"content_policy_blocked": ErrContentFilter, "incomplete_response": ErrEmptyResponse, "format_error": ErrInvalidRequest,
	"role_alternation": ErrInvalidRequest, "invalid_encrypted_content": ErrInvalidRequest, "multimodal_tool_content_unsupported": ErrInvalidRequest,
	"reasoning_mandatory": ErrInvalidRequest, "thinking_signature": ErrInvalidRequest, "oauth_long_context_beta_forbidden": ErrInvalidRequest,
	"llama_cpp_grammar_pattern": ErrInvalidRequest, "cancelled": ErrUnknown, "unknown": ErrUnknown,
}

// JSON schema a reply must follow
type Format struct {
	Name   string
	Schema map[string]any
	Strict bool
}

type Response struct {
	Message      *sessionsv1.Message
	FinishReason string
	// Tokens the request took, nil when the provider said nothing of them
	Usage *Usage
	// Headers of the provider's answer, such as the rate limits it states
	Header http.Header
}

// Tokens a request took: the input not read from a cache, the input read from and written to the provider's cache, the output, and the
// part of the output spent reasoning
type Usage struct {
	Input      int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
	Reasoning  int64
	// Tier or speed that served the request when not the standard one, such as fast or ultrafast, which bills at its own prices
	Tier string
}

// Every token of the prompt, cached or not
func (u Usage) Prompt() int64 {
	return u.Input + u.CacheRead + u.CacheWrite
}

// Usage of two requests together
func (u Usage) Add(other Usage) Usage {
	return Usage{Input: u.Input + other.Input, CacheRead: u.CacheRead + other.CacheRead, CacheWrite: u.CacheWrite + other.CacheWrite,
		Output: u.Output + other.Output, Reasoning: u.Reasoning + other.Reasoning}
}

type EventKind string

const (
	EventTextDelta      EventKind = "text_delta"
	EventReasoningDelta EventKind = "reasoning_delta"
	// Line the person is shown about the request, such as a stalled stream being dropped and asked again
	EventStatus EventKind = "status"
	// Line shown in place while the request waits on the provider, replaced by the next one and cleared by an empty one
	EventWait EventKind = "wait"
	// What the request is doing, for the session's liveness: never shown in the transcript
	EventActivity EventKind = "activity"
	// The model began a tool call of the name, its arguments still streaming
	EventToolStart EventKind = "tool_start"
)

type StreamEvent struct {
	Kind EventKind
	Text string
}

// Whether the event delivers part of the reply, text or reasoning, which a request sent again would deliver twice
func (e StreamEvent) Delivers() bool {
	return e.Kind == EventTextDelta || e.Kind == EventReasoningDelta
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
	// The account is out of credit or quota
	ErrQuota   ErrorKind = "quota"
	ErrUnknown ErrorKind = "unknown"
)

type APIError struct {
	Provider   string
	Model      string
	StatusCode int
	Message    string
	RetryAfter time.Duration
	Kind       ErrorKind
	Err        error
	// Error object of the provider's reply, with the type and code it names; nil and empty when it had none
	Body map[string]any
	Type string
	Code string
	// Reason a plugin classified the failure with, empty when none did, and what it decided: whether the request is tried again, the context
	// compressed, the account rotated past and the session moved to its fallback, each nil to leave it to the kind; with what it adds of the
	// failure
	Reason                            string
	Retry, Compress, Rotate, Fallback *bool
	Context                           map[string]any
	// The message is icebath's own account of what happened and what to do, such as a request it gave up waiting on, rather than the
	// provider's words
	Own bool
}

func (e *APIError) Error() string {
	if e.Own && strings.TrimSpace(e.Message) != "" {
		message := strings.TrimSpace(e.Message)
		if !strings.ContainsAny(message[len(message)-1:], ".!?") {
			message += "."
		}
		return message
	}
	who, model := cmp.Or(e.Provider, "The provider"), cmp.Or(e.Model, "the model")
	status := ""
	if e.StatusCode > 0 {
		status = fmt.Sprintf(", with HTTP status %d", e.StatusCode)
	}
	var lead string
	switch e.Kind {
	case ErrAuth:
		lead = who + " did not accept the account" + status + ". Check its API key or sign-in in /providers."
	case ErrRateLimit:
		lead = who + " is limiting requests for " + model + status + ". Wait a moment, then send your message again."
	case ErrQuota:
		lead = who + " says the account is out of credit or usage for " + model + status + "."
	case ErrContextOverflow:
		lead = "The session is too long for " + model + " on " + who + status + ". Type /compress to summarize it, or /new to start a new one."
	case ErrModelNotFound:
		lead = who + " does not have the model " + model + status + ". Pick another with /model."
	case ErrServer:
		lead = who + " had a failure of its own" + status + ". Send your message again shortly."
	case ErrNetwork:
		lead = "icebath could not get an answer from " + who + status + ". Check your connection, then send your message again."
	case ErrContentFilter:
		lead = who + "'s safety filter refused the request" + status + ". Rephrase your message, or pick another model with /model."
	case ErrEmptyResponse:
		lead = who + " sent back an empty reply" + status + ". Send your message again."
	case ErrInvalidRequest:
		lead = who + " refused the request for " + model + status + "."
	default:
		lead = who + " refused the request for " + model + status + "."
	}
	detail := strings.TrimSpace(e.Message)
	if e.StatusCode > 0 {
		detail = strings.TrimSpace(strings.TrimPrefix(detail, fmt.Sprintf("HTTP %d:", e.StatusCode)))
	}
	if detail == "" {
		return lead
	}
	first, rest, _ := strings.Cut(detail, "\n")
	if first = strings.TrimSpace(first); first != "" && !strings.ContainsAny(first[len(first)-1:], ".!?") {
		first += "."
	}
	if rest != "" {
		first += "\n" + rest
	}
	return lead + "\n" + who + " said: " + first
}

func (e *APIError) Unwrap() error {
	return e.Err
}

func (e *APIError) Retryable() bool {
	if e.Retry != nil {
		return *e.Retry
	}
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

// Variables that name the program a kind that runs one starts and its arguments, which no generic writer may set, as they decide what runs
func (r *Registry) LaunchEnv() []string {
	var names []string
	for _, spec := range r.Specs() {
		if spec.AuthType != AuthTypeExternalProcess {
			continue
		}
		for _, name := range append(slices.Clone(spec.ProcessCommandEnvVars), spec.ProcessArgsEnvVar) {
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
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
