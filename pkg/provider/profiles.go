package provider

import (
	"context"
	"net"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Wire protocols routed by providers whose adapters register their own mode constants
const (
	modeBedrockConverse APIMode = "bedrock_converse"
	modeCodexAppServer  APIMode = "codex_app_server"
)

// Hermes auth_type literals a profile declares
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

// Profile describes one built in inference provider
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
	LazyHeaders                *LazyHeaders
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

	// Responses wire flags stored so the zero value keeps the upstream defaults
	ResponsesDropReplayItemIDs       bool
	ResponsesOmitPromptCacheKey      bool
	ResponsesIssuer                  string
	ResponsesDistrustCompletedStatus bool
	ResponsesNoNativeCompaction      bool
	ResponsesReasoningFromProfile    bool

	AuthHandler           func(action string, args []string, provider string) (bool, error)
	RefreshCredential     func(entry PooledCredential) (map[string]any, error)
	ClassifyAPIError      ClassifierHook
	AccountStatus         func(ctx context.Context) (any, error)
	StartDeviceLogin      func(ctx context.Context) (any, error)
	PollDeviceLogin       func(ctx context.Context, state any) (any, error)
	SaveDeviceLogin       func(ctx context.Context, result any) error
	ModelSetupFlow        func(ctx context.Context, cfg any, currentModel string, args any) (any, error)
	SetupReasoningEfforts func(model string) any
	PickerCatalog         func(ctx context.Context, forceRefresh bool) ([]string, bool)

	// Hook overrides, a nil hook runs the upstream default body
	FetchAccountUsageHook               func(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error)
	ResolveAuxModelHook                 func(ctx context.Context, vision bool) string
	GetHostnameHook                     func() string
	PrepareMessagesHook                 func(messages []map[string]any) []map[string]any
	BuildExtraBodyHook                  func(sessionID string, context map[string]any) map[string]any
	DefaultReasoningConfigHook          func(model string) *Reasoning
	BuildAPIKwargsExtrasHook            func(reasoning *Reasoning, context map[string]any) (map[string]any, map[string]any)
	BuildClientKwargsExtrasHook         func(context map[string]any) map[string]any
	DefaultVisionModelHook              func(ctx context.Context) string
	GetModelContextLengthHook           ProfileContextLengthHook
	GetUsageCostHook                    UsageCostHook
	GetMaxTokensHook                    func(model string) int
	SupportedReasoningEffortsHook       func(ctx context.Context, model string) ([]string, bool, error)
	APIModeForModelHook                 PerModelAPIModeHook
	ClientHeadersHook                   func(baseURL, apiKey string, hasImages bool) map[string]string
	RequestHeadersHook                  func(baseURL, model string, userInitiated bool) map[string]string
	MaxTokensParamHook                  func(baseURL, model string) string
	ListedHook                          func(ctx context.Context) (bool, error)
	CreateClientHook                    func(ctx context.Context, clientKwargs map[string]any) (Client, error)
	SetupStatusHook                     func(ctx context.Context, kwargs map[string]any) (map[string]any, error)
	DiscoverModelsHook                  func(ctx context.Context, kwargs map[string]any) ([]map[string]any, error)
	FetchModelsHook                     func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool)
	LiveContextLengthHook               ProfileLiveContextLengthHook
	NormalizeModelIDHook                ModelIDNormalizer
	ResolveAPIKeySecretHook             APIKeySecretResolver
	RuntimeBaseURLHook                  RuntimeBaseURLResolver
	PreparePoolEntryHook                func(entry *PooledCredential, apiKey string) string
	SeedPoolCredentialsHook             func(suppressedSources map[string]bool, explicitlyConfigured bool) []map[string]any
	RemoveCredentialSourceHook          func(source string) ([]string, []string, bool)
	RefreshRuntimeCredentialsHook       func(ctx context.Context, reason string) (apiKey, baseURL string, ok bool, err error)
	IsStaleCredentialErrorHook          func(statusCode int, message string) bool
	CredentialRefreshNoticeHook         func(reason string) string
	RequestRejectionHintHook            func(statusCode int, baseURL string) []string
	ExternalAuthEvidenceHook            func() (verified bool, source string, ok bool)
	MissingCredentialsHintHook          func() any
	ReasoningPayloadHook                func(model string, r Reasoning, baseURL string) map[string]any
	ReplayMessageCarriersHook           func(values map[string]any, model, baseURL string) map[string]any
	ReplaysFunctionThoughtSignatureHook func(model, baseURL string) bool
	ProbeModelsHook                     func(ctx context.Context, baseURL, apiKey string, timeout time.Duration) *ModelsProbe
	InvalidateCatalogCacheHook          func()
}

// Overlay fields for a provider's models.dev keyed definition
type IcebathOverlay struct {
	Transport       string
	IsAggregator    bool
	AuthType        string
	ExtraEnvVars    []string
	BaseURLOverride string
	BaseURLEnvVar   string
}

// Result of a provider owned models probe, IDs nil with Listed false when the probe failed
type ModelsProbe struct {
	IDs             []string
	Listed          bool
	ProbedURL       string
	ResolvedBaseURL string
}

// AuthKindOf maps a hermes auth_type literal onto the route credential style
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

// Label returns the human readable provider name
func (p Profile) Label() string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.Name
}

// KeyEnvNames returns the API key env names without URL shaped entries
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

var (
	staticAliasOnce  sync.Once
	staticAliasIndex map[string]string
	aliasMemoMu      sync.Mutex
	aliasMemo        = map[string]aliasMemoEntry{}
)

type aliasMemoEntry struct {
	generation int64
	index      map[string]string
}

// Static alias table keyed by alias
func staticAliases() map[string]string {
	staticAliasOnce.Do(func() {
		staticAliasIndex = map[string]string{}
		for canon, aliases := range staticAliasGroups {
			for _, a := range aliases {
				staticAliasIndex[a] = canon
			}
		}
	})
	return staticAliasIndex
}

// Aliases of the bound home's registered profiles, the first profile claiming an alias keeping it
func profileAliasIndex(ctx context.Context) map[string]string {
	key, gen := BoundRegistryKey(ctx)
	aliasMemoMu.Lock()
	if m, ok := aliasMemo[key]; ok && m.generation == gen {
		aliasMemoMu.Unlock()
		return m.index
	}
	aliasMemoMu.Unlock()
	index := map[string]string{}
	for _, p := range listProviders(ctx) {
		for _, a := range p.Aliases {
			if _, taken := index[a]; !taken {
				index[a] = p.Name
			}
		}
	}
	aliasMemoMu.Lock()
	aliasMemo[key] = aliasMemoEntry{gen, index}
	aliasMemoMu.Unlock()
	return index
}

// Profiles returns the registry rows in priority order followed by registered profiles without a row
func Profiles() []Profile { return profilesCtx(context.Background()) }

func profilesCtx(ctx context.Context) []Profile {
	seen := map[string]bool{}
	var out []Profile
	for _, row := range canonicalRegistryRows(ctx) {
		seen[row.Name] = true
		out = append(out, mergedRegistryProfile(ctx, row))
	}
	for _, p := range listProviders(ctx) {
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, *p)
		}
	}
	return out
}

// ProfileFor resolves a provider id or alias to its registry row, else to its registered profile
func ProfileFor(name string) (Profile, bool) { return profileForCtx(context.Background(), name) }

func profileForCtx(ctx context.Context, name string) (Profile, bool) {
	key := normalizeProviderCtx(ctx, name)
	if row := registryLookup(ctx, key); row != nil {
		return mergedRegistryProfile(ctx, row), true
	}
	user, shared := boundProfileResolver(ctx)(key)
	if user != nil {
		return *user, true
	}
	if shared != nil {
		return *shared, true
	}
	return Profile{}, false
}

// NormalizeProvider lowercases a provider name and collapses every alias onto its canonical id
func NormalizeProvider(name string) string { return normalizeProviderCtx(context.Background(), name) }

func normalizeProviderCtx(ctx context.Context, name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if canon, ok := staticAliases()[key]; ok {
		return canon
	}
	if canon, ok := profileAliasIndex(ctx)[key]; ok {
		return canon
	}
	return key
}

// IsLocalAlias reports whether the name is one of the local server aliases routed through custom
func IsLocalAlias(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp", "lmstudio", "lm-studio", "lm_studio":
		return true
	}
	return false
}

// IsLlamaCppAlias reports whether the name addresses the managed llama.cpp runtime
func IsLlamaCppAlias(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "llamacpp", "llama.cpp", "llama-cpp":
		return true
	}
	return false
}

// Label returns the display name for a provider id, alias, or custom slug
func Label(providerID string) string {
	canon := NormalizeProvider(providerID)
	if strings.HasPrefix(canon, "custom:") {
		return strings.TrimPrefix(canon, "custom:")
	}
	if p, ok := ProfileFor(canon); ok {
		return p.Label()
	}
	return canon
}

// Hostname returns the lowercase host of a URL without port, userinfo, or trailing dot
func Hostname(rawURL string) string {
	host, err := pyurl.BaseURLHostname(rawURL)
	if err != nil {
		return ""
	}
	return host
}

// URLPath returns the URL path lowercased without a trailing slash, empty for a bare host or a bad netloc
func URLPath(rawURL string) string {
	path, err := pyurl.BaseURLPath(rawURL)
	if err != nil {
		return ""
	}
	return path
}

// URLPort returns the port of a URL or -1 when it cannot be parsed
func URLPort(rawURL string) int {
	raw := strings.TrimSpace(rawURL)
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return -1
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			return 443
		case "http":
			return 80
		}
		return 0
	}
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return -1
		}
	}
	return n
}

// HostMatches reports whether the URL host equals the domain or is a subdomain of it
func HostMatches(rawURL, domain string) bool {
	host := Hostname(rawURL)
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// IsLoopbackHost reports whether the host names the local machine
func IsLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	switch h {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// IsOfficialOpenAIHost reports whether the URL points at api.openai.com or a data residency subdomain
func IsOfficialOpenAIHost(rawURL string) bool {
	return HostMatches(rawURL, "api.openai.com")
}

// IsOllamaEndpoint reports whether the URL carries an explicit Ollama signature
func IsOllamaEndpoint(rawURL string) bool {
	if strings.TrimSpace(rawURL) == "" {
		return false
	}
	port := URLPort(rawURL)
	if port == -1 {
		return false
	}
	if port == 11434 {
		return true
	}
	host := Hostname(rawURL)
	if host == "" {
		return false
	}
	if host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "ollama" {
			return true
		}
	}
	return false
}

// NormalizeActualBaseURL gives hosted or loopback Actual roots their /v1 surface and the default when empty
func NormalizeActualBaseURL(baseURL string) string {
	return normalizeActualRuntimeBaseURL(baseURL)
}

// Hosts that accept exactly one wire protocol
var responsesNativeHosts = map[string]bool{"api.meta.ai": true, "api.router.com": true}

// HostMandatedMode returns the wire protocol an endpoint requires or empty when any works
func HostMandatedMode(baseURL string) APIMode {
	if strings.TrimSpace(baseURL) == "" {
		return ""
	}
	lower := strings.ToLower(strings.TrimRight(baseURL, "/"))
	host := Hostname(baseURL)
	switch {
	case host == "api.actual.inc":
		return ModeChatCompletions
	case host == "api.kimi.com" && strings.Contains(lower, "/coding"):
		return ModeAnthropicMessages
	case host == "api.anthropic.com" || strings.HasSuffix(lower, "/anthropic"):
		return ModeAnthropicMessages
	case IsOfficialOpenAIHost(baseURL) || responsesNativeHosts[host]:
		return ModeCodexResponses
	case strings.HasPrefix(host, "bedrock-runtime.") && HostMatches(baseURL, "amazonaws.com"):
		return modeBedrockConverse
	}
	return ""
}

// DetectModeForURL infers the wire protocol from the endpoint shape or returns empty
func DetectModeForURL(baseURL string) APIMode {
	host := Hostname(baseURL)
	switch host {
	case "api.x.ai", "api.meta.ai", "api.router.com":
		return ModeCodexResponses
	case "api.actual.inc":
		return ModeChatCompletions
	case "api.anthropic.com":
		return ModeAnthropicMessages
	}
	if IsOfficialOpenAIHost(baseURL) {
		return ModeCodexResponses
	}
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	path := URLPath(normalized)
	if strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") || (host == "api.kimi.com" && strings.Contains(normalized, "/coding")) {
		return ModeAnthropicMessages
	}
	return ""
}

// NousMode picks the Portal wire for a model with anthropic slugs riding chat unless native is configured
func NousMode(model, anthropicWire string) APIMode {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/") && strings.ToLower(strings.TrimSpace(anthropicWire)) == "native" {
		return ModeAnthropicMessages
	}
	return ModeChatCompletions
}

// ParseMode canonicalizes legacy api_mode spellings and rejects unknown ones
func ParseMode(raw string) APIMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "chat_completions", "chat-completions", "chat", "openai", "openai_chat", "openai-chat", "completions":
		return ModeChatCompletions
	case "codex_responses", "codex-responses", "responses", "openai_responses", "openai-responses", "codex":
		return ModeCodexResponses
	case "anthropic_messages", "anthropic-messages", "anthropic", "messages", "claude":
		return ModeAnthropicMessages
	case "gemini_native", "gemini-native", "gemini", "google":
		return ModeGeminiNative
	case "bedrock_converse", "bedrock-converse", "bedrock", "converse":
		return modeBedrockConverse
	case "codex_app_server", "codex-app-server", "app_server":
		return modeCodexAppServer
	}
	return ""
}

// UserAgent builds the attribution header value sent to aggregators
func UserAgent(product string) string {
	return product + "/" + Version + " (" + runtime.GOOS + "; " + runtime.GOARCH + "; go)"
}

// Version is the agent version stamped into attribution headers
var Version = "dev"

// Attribution headers aggregators use to credit the agent
func attributionHeaders(product string) map[string]string {
	return map[string]string{
		"HTTP-Referer": "https://github.com/nickheyer/icebath",
		"X-Title":      "Icebath Agent",
		"User-Agent":   UserAgent(product),
	}
}
