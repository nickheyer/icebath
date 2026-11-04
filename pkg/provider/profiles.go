package provider

import (
	"net"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	FlatReseller               bool
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
	profilesOnce  sync.Once
	profilesList  []Profile
	profilesByKey map[string]int
	aliasIndex    map[string]string
)

func buildProfiles() {
	profilesList = profileTable()
	profilesByKey = make(map[string]int, len(profilesList))
	aliasIndex = make(map[string]string, len(profilesList)*4)
	for i := range profilesList {
		p := &profilesList[i]
		if p.Auth == "" {
			p.Auth = AuthKindOf(p.AuthType)
		}
		if p.AuthType == "" {
			p.AuthType = AuthTypeAPIKey
		}
		if p.Mode == "" {
			p.Mode = ModeChatCompletions
		}
		profilesByKey[p.Name] = i
	}
	for canon, aliases := range staticAliasGroups {
		for _, a := range aliases {
			aliasIndex[a] = canon
		}
	}
	for _, p := range profilesList {
		for _, a := range p.Aliases {
			if _, taken := aliasIndex[a]; !taken {
				aliasIndex[a] = p.Name
			}
		}
	}
}

// Profiles returns every built in provider profile sorted by name
func Profiles() []Profile {
	profilesOnce.Do(buildProfiles)
	out := make([]Profile, len(profilesList))
	copy(out, profilesList)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProfileFor resolves a provider id or alias to its profile
func ProfileFor(name string) (Profile, bool) {
	profilesOnce.Do(buildProfiles)
	key := NormalizeProvider(name)
	i, ok := profilesByKey[key]
	if !ok {
		return Profile{}, false
	}
	return profilesList[i], true
}

// NormalizeProvider lowercases a provider name and collapses every alias onto its canonical id
func NormalizeProvider(name string) string {
	profilesOnce.Do(buildProfiles)
	key := strings.ToLower(strings.TrimSpace(name))
	if canon, ok := aliasIndex[key]; ok {
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

// IsAggregator reports whether the provider fronts many vendors under one key
func IsAggregator(providerName string) bool {
	canon := NormalizeProvider(providerName)
	if strings.HasPrefix(canon, "custom:") {
		return true
	}
	p, ok := ProfileFor(canon)
	return ok && p.Aggregator
}

// IsRoutingAggregator excludes flat namespace resellers whose catalog is first party
func IsRoutingAggregator(providerName string) bool {
	canon := NormalizeProvider(providerName)
	if p, ok := ProfileFor(canon); ok && p.FlatReseller {
		return false
	}
	return IsAggregator(canon)
}

// Hostname returns the lowercase host of a URL without port, userinfo, or trailing dot
func Hostname(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// URLPath returns the URL path lowercased without a trailing slash
func URLPath(rawURL string) string {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.ToLower(u.Path), "/")
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

// IsActualRoute identifies Actual Computer by provider alias or hosted endpoint
func IsActualRoute(providerName, baseURL string) bool {
	return NormalizeProvider(providerName) == "actual" || Hostname(baseURL) == "api.actual.inc"
}

// NormalizeActualBaseURL pins hosted Actual routes onto the /v1 root
func NormalizeActualBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if Hostname(trimmed) != "api.actual.inc" {
		return trimmed
	}
	return "https://api.actual.inc/v1"
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

// DetermineMode picks the wire protocol from host mandate, Nous dual wire, then the profile transport
func DetermineMode(providerName, baseURL, model, nousWire string) APIMode {
	if IsActualRoute(providerName, baseURL) {
		return ModeChatCompletions
	}
	if mandated := HostMandatedMode(baseURL); mandated != "" {
		return mandated
	}
	canon := NormalizeProvider(providerName)
	if canon == "nous" {
		return NousMode(model, nousWire)
	}
	if p, ok := ProfileFor(canon); ok {
		if p.PerModelMode != nil && model != "" {
			if m := p.PerModelMode(model); m != "" {
				return m
			}
		}
		return p.Mode
	}
	if canon == "bedrock" {
		return modeBedrockConverse
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
