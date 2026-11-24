package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// ScopedCredentialSource narrows a pool to the rows a custom entry owns before selecting one
type ScopedCredentialSource interface {
	CredentialForEntry(poolKey string, entry map[string]any) (Credential, bool)
}

// ModelCredentialSource selects a login that is not benched for the target model
type ModelCredentialSource interface {
	CredentialForModel(provider, model string) (Credential, bool)
	TokenBenchedForModel(provider, token, model string) bool
}

// SignedInSource names the provider whose stored logins report signed in, the one resolution falls to when nothing else selects a provider
type SignedInSource interface {
	SignedInProvider() string
}

// One resolved provider runtime, the dict handed to the agent
type Runtime struct {
	Provider          string
	APIMode           APIMode
	BaseURL           string
	APIKey            string
	TokenSource       func(ctx context.Context) (string, error)
	Source            string
	RequestedProvider string
	Model             string
	KeyEnv            string
	ProviderKey       string
	KeyCmd            string
	ExtraHeaders      map[string]string
	RequestOverrides  map[string]any
	Capabilities      map[string]bool
	AuthMode          string
	Entra             map[string]any
	Region            string
	BedrockOpenAI     bool
	BedrockAnthropic  bool
	GuardrailConfig   map[string]any
	Command           string
	Args              []string
	AuthError         *AuthError
	Expiry            map[string]any
	CredentialPool    *Pool
}

// Credential as the runtime dict holds it, the callable token source when no static key was resolved
func (rt *Runtime) APIKeyValue() any {
	if rt.APIKey == "" && rt.TokenSource != nil {
		return rt.TokenSource
	}
	return rt.APIKey
}

// Route converts the runtime into the wire route the adapters consume
func (rt *Runtime) Route() Route {
	route := Route{
		Provider: rt.Provider, Model: rt.Model, BaseURL: rt.BaseURL, APIKey: rt.APIKey, Mode: rt.APIMode,
		Region: rt.Region, TokenSource: rt.TokenSource, Auth: AuthAPIKey,
	}
	if len(rt.ExtraHeaders) > 0 {
		route.Headers = copyStringMap(rt.ExtraHeaders)
	}
	if eb, ok := rt.RequestOverrides["extra_body"].(map[string]any); ok && len(eb) > 0 {
		route.ExtraBody = copyAnyMap(eb)
	}
	if p, ok := ProfileFor(rt.Provider); ok {
		route.Auth = AuthKindOf(p.AuthType)
	}
	switch {
	case rt.Provider == "bedrock" && !rt.BedrockOpenAI:
		route.Auth = AuthAWSSigV4
	case rt.Provider == "vertex":
		route.Auth = AuthGCPOAuth
	case rt.TokenSource != nil:
		route.Auth = AuthOAuth
	case rt.APIKey == NoKeyPlaceholder:
		route.Auth = AuthNone
	}
	if rt.AuthMode == "entra_id" {
		route.Auth = AuthOAuth
	}
	route.Local = isLoopbackHostname(Hostname(rt.BaseURL)) || rt.Source == "local-runtime"
	return route
}

// resolver carries the context, secrets, home, and logins one resolution walks with, its settings read for the context bound to the home
type resolver struct {
	ctx     context.Context
	secrets *config.Secrets
	home    string
	creds   CredentialSource
	// model.default with a local server's single model filled in, once read
	model     string
	modelRead bool
	http      *http.Client
}

// Resolver over a home's secrets and logins whose settings reads follow ctx
func newResolver(ctx context.Context, secrets *config.Secrets, h string, creds CredentialSource) *resolver {
	if ctx == nil {
		ctx = context.Background()
	}
	return &resolver{ctx: ctx, secrets: secrets, home: h, creds: creds, http: &http.Client{}}
}

// Call context settings reads and profile hooks go through, the resolver's home bound onto its context
func (r *resolver) hookCtx() context.Context { return ctxForHome(r.ctx, r.home) }

// Providers setting in effect for the resolution
func (r *resolver) providers() map[string]any { return settings.Providers.Get(r.hookCtx()) }

// model.provider in effect for the resolution, lower case
func (r *resolver) cfgProvider() string {
	return strings.ToLower(strings.TrimSpace(settings.ModelProvider.Get(r.hookCtx())))
}

// model.base_url in effect for the resolution
func (r *resolver) cfgBaseURL() string {
	return strings.TrimSpace(settings.ModelBaseURL.Get(r.hookCtx()))
}

// model.api_mode in effect for the resolution when it names a known mode
func (r *resolver) cfgAPIMode() APIMode {
	return r.parseAPIMode(settings.ModelAPIMode.Get(r.hookCtx()))
}

// Process environment value under the get_secret rules, a scoped routed profile never reading another profile's value
func (r *resolver) processEnv(name string) string {
	if boundScope(r.ctx) != nil && !IsGlobalEnv(name) && r.servesRoutedProfile() {
		return CurrentSecretScope(r.ctx)[name]
	}
	v, _ := SecretScopeFrom(r.ctx).GetSecret(name)
	return v
}

// Secret by env var name from the profile .env then the process environment the profile may read
func (r *resolver) secret(name string) string {
	if name == "" {
		return ""
	}
	if r.secrets != nil {
		if v, ok := r.secrets.File()[name]; ok {
			return v
		}
	}
	return r.processEnv(name)
}

// Credential preferring the .env file, with an unresolved op reference yielding to the environment
func (r *resolver) envPreferDotenv(name string) string {
	raw := ""
	if r.secrets != nil {
		raw = r.secrets.File()[name]
	}
	if strings.HasPrefix(strings.TrimLeft(raw, " \t"), "op://") {
		if scoped := r.processEnv(name); scoped != "" {
			return scoped
		}
	}
	if raw != "" {
		return raw
	}
	return r.processEnv(name)
}

// The profile .env assignments
func (r *resolver) fileEnv() map[string]string {
	if r.secrets == nil {
		return map[string]string{}
	}
	return r.secrets.File()
}

// ---- model config ----

var localModelHosts = map[string]bool{"localhost": true, "127.0.0.1": true}

// Model name of a local server at baseURL when exactly one model is loaded, empty otherwise
func AutoDetectLocalModel(ctx context.Context, baseURL string) string {
	return (&resolver{ctx: ctx, http: &http.Client{}}).autoDetectLocalModel(baseURL)
}

// Queries a local server for its model name when exactly one model is loaded
func (r *resolver) autoDetectLocalModel(baseURL string) string {
	if baseURL == "" {
		return ""
	}
	u := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, u+"/models", nil)
	if err != nil {
		return ""
	}
	client := *r.http
	client.Timeout = 5e9
	resp, err := client.Do(req)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Auto-detect model from %s failed: %v", baseURL, err))
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if len(payload.Data) == 1 && payload.Data[0].ID != "" {
		return payload.Data[0].ID
	}
	return ""
}

// model.default in effect, or the single model a local server at model.base_url serves when none is set
func (r *resolver) cfgModel() string {
	if r.modelRead {
		return r.model
	}
	model := strings.TrimSpace(settings.Model.Get(r.hookCtx()))
	if baseURL := r.cfgBaseURL(); model == "" && baseURL != "" && localModelHosts[Hostname(baseURL)] {
		model = r.autoDetectLocalModel(baseURL)
	}
	r.model, r.modelRead = model, true
	return model
}

// Provider request from the explicit arg, then config, then the bound client or secret ICEBATH_INFERENCE_PROVIDER, else auto
func (r *resolver) resolveRequestedProvider(requested string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.ToLower(strings.TrimSpace(requested))
	}
	if cfgProvider := r.cfgProvider(); cfgProvider != "" {
		return cfgProvider
	}
	var requestedEnv string
	if client := state.BoundClientEnv(r.ctx); client != nil {
		requestedEnv = client["ICEBATH_INFERENCE_PROVIDER"]
	} else {
		requestedEnv = r.secret("ICEBATH_INFERENCE_PROVIDER")
	}
	if env := strings.ToLower(strings.TrimSpace(requestedEnv)); env != "" {
		return env
	}
	return "auto"
}

// Explicit provider, then the config provider, then ICEBATH_INFERENCE_PROVIDER, else auto
func ResolveRequestedProvider(ctx context.Context, h, requested string) (string, error) {
	r, err := strictHomeResolver(ctx, h)
	if err != nil {
		return "", err
	}
	return r.resolveRequestedProvider(requested), nil
}

// ---- api mode detection ----

var validAPIModes = map[string]bool{
	"chat_completions": true, "codex_responses": true, "anthropic_messages": true, "bedrock_converse": true, "codex_app_server": true,
}

// Hosts that only speak one wire protocol
var hostMandatedAPIModes = map[string]APIMode{
	"api.x.ai": ModeCodexResponses, "api.meta.ai": ModeCodexResponses, "api.actual.inc": ModeChatCompletions,
	"api.router.com": ModeCodexResponses, "api.anthropic.com": ModeAnthropicMessages,
}

// Auto detected api mode from a base URL, exact host matches first, then anthropic style paths
func detectAPIModeForURL(baseURL string) APIMode {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	hostname := Hostname(baseURL)
	if mandated, ok := hostMandatedAPIModes[hostname]; ok {
		return mandated
	}
	if IsOfficialOpenAIHost(baseURL) {
		return ModeCodexResponses
	}
	path := URLPath(normalized)
	if strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") || (hostname == "api.kimi.com" && strings.Contains(normalized, "/coding")) {
		return ModeAnthropicMessages
	}
	return ""
}

// DetectAPIModeForURL is the api mode a base url implies by its path and host, empty when none
func DetectAPIModeForURL(baseURL string) APIMode { return detectAPIModeForURL(baseURL) }

// Validated api mode from config, the built in modes and registered dialects accepted
func (r *resolver) parseAPIMode(raw any) APIMode {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	normalized := strings.ToLower(strings.TrimSpace(s))
	if normalized == "" {
		return ""
	}
	if validAPIModes[normalized] || RegisteredAPIModes()[normalized] {
		return APIMode(normalized)
	}
	return ""
}

// Api mode when no explicit mode applies, URL detection, then the declared transport, then chat
func (r *resolver) fallbackAPIMode(provider, baseURL, model string) APIMode {
	if isActualRouteCtx(r.ctx, provider, baseURL) {
		return ModeChatCompletions
	}
	if detected := detectAPIModeForURL(baseURL); detected != "" {
		return detected
	}
	declared := determineModeCtx(r.ctx, provider, baseURL, model)
	if declared == ModeAnthropicMessages && !onDeclaredAnthropicEndpoint(r.ctx, provider, baseURL) {
		return ModeChatCompletions
	}
	return declared
}

// Whether the base URL is the provider's own anthropic protocol endpoint
func onDeclaredAnthropicEndpoint(ctx context.Context, provider, baseURL string) bool {
	defaultURL := ""
	if pdef := GetProviderDef(ctx, provider, false); pdef != nil {
		defaultURL = strings.TrimSpace(pdef.BaseURL)
	}
	defaultPath := strings.TrimSuffix(URLPath(defaultURL), "/v1")
	if defaultPath == "" || strings.TrimSpace(baseURL) == "" {
		return true
	}
	if !HostMatches(baseURL, Hostname(defaultURL)) {
		return false
	}
	path := URLPath(baseURL)
	return path == "" || path == defaultPath || strings.HasPrefix(path, defaultPath+"/")
}

// Api mode for legacy plain custom endpoints, only direct OpenAI shaped URLs imply Responses
func (r *resolver) resolvePlainCustomAPIMode(baseURL string) APIMode {
	if isActualRouteCtx(r.ctx, "", baseURL) {
		return ModeChatCompletions
	}
	configured := r.cfgAPIMode()
	detected := detectAPIModeForURL(baseURL)
	if configured == ModeCodexResponses && detected != ModeCodexResponses {
		target := baseURL
		if target == "" {
			target = "(unknown)"
		}
		pkgLog().Info("Ignoring persisted custom api_mode=codex_responses for non-OpenAI endpoint " + target)
		configured = ""
	}
	if configured != "" {
		return configured
	}
	if detected != "" {
		return detected
	}
	return ModeChatCompletions
}

// Registry profile, which excludes the openrouter and custom pseudo providers
func registryProfile(provider string) (Profile, bool) {
	return registryProfileCtx(context.Background(), provider)
}

// Registry profile of the home bound to ctx, the openrouter and custom pseudo providers excluded
func registryProfileCtx(ctx context.Context, provider string) (Profile, bool) {
	key := strings.ToLower(strings.TrimSpace(provider))
	if key == "openrouter" || key == "custom" || key == "" {
		return Profile{}, false
	}
	row := registryLookup(ctx, normalizeProviderCtx(ctx, key))
	if row == nil || row.Name == "openrouter" || row.Name == "custom" {
		return Profile{}, false
	}
	return mergedRegistryProfile(ctx, row), true
}

// Profile aliases share one registry id, unrelated routes stay distinct
func sameRegisteredProvider(ctx context.Context, provider, configuredProvider string) bool {
	if provider == configuredProvider {
		return true
	}
	p, ok1 := registryProfileCtx(ctx, provider)
	c, ok2 := registryProfileCtx(ctx, configuredProvider)
	return ok1 && ok2 && p.Name == c.Name
}

// Whether a persisted api_mode may be honoured for the provider
func providerSupportsExplicitAPIMode(ctx context.Context, provider, configuredProvider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	c := strings.ToLower(strings.TrimSpace(configuredProvider))
	if c == "" {
		return true
	}
	if p == "custom" {
		return c == "custom" || strings.HasPrefix(c, "custom:")
	}
	return sameRegisteredProvider(ctx, p, c)
}

// Persisted model.api_mode when valid and recorded for this provider
func (r *resolver) configuredAPIMode(provider string) APIMode {
	configured := r.cfgAPIMode()
	if configured != "" && providerSupportsExplicitAPIMode(r.ctx, provider, r.cfgProvider()) {
		return configured
	}
	return ""
}

// The caller's target model beats the configured one
func (r *resolver) effectiveModel(targetModel string) string {
	if targetModel != "" {
		return targetModel
	}
	return r.cfgModel()
}

// PerModelAPIModeHook answers the wire for one model with a key or no key, empty for the declared mode
type PerModelAPIModeHook func(model string, apiKey *string) APIMode

// perModelHookProfile is a provider whose profile routes api mode per model through its hook
type perModelHookProfile struct {
	name string
	mode APIMode
	hook PerModelAPIModeHook
}

// The provider's profile when it routes api mode per model through a registered hook
func perModelProfile(provider string) *perModelHookProfile {
	return perModelProfileCtx(context.Background(), provider)
}

// Per model hook profile of the home bound to ctx
func perModelProfileCtx(ctx context.Context, provider string) *perModelHookProfile {
	if provider == "" {
		return nil
	}
	var hook PerModelAPIModeHook
	if p := getProviderProfile(ctx, provider); p != nil {
		hook = p.APIModeForModelHook
	}
	if hook == nil {
		return nil
	}
	declared := ModeChatCompletions
	if p, ok := profileForCtx(ctx, provider); ok && p.Mode != "" {
		declared = p.Mode
	}
	return &perModelHookProfile{name: provider, mode: declared, hook: hook}
}

// Providers whose raising api mode hook was already logged in this process
var (
	apiModeWarnedMu sync.Mutex
	apiModeWarned   = map[string]bool{}
)

// The profile's per model wire answer, a raising hook answering the declared mode and warning once per provider
func modelAPIMode(ctx context.Context, profile *perModelHookProfile, model string, apiKey *string) (mode APIMode) {
	defer func() {
		if rec := recover(); rec != nil {
			apiModeWarnedMu.Lock()
			first := !apiModeWarned[profile.name]
			apiModeWarned[profile.name] = true
			apiModeWarnedMu.Unlock()
			if first {
				pkgLog().WarnContext(ctx, fmt.Sprintf("provider %s: api_mode_for_model failed; using its declared api_mode", profile.name),
					"exc_info", fmt.Sprintf("%v\n%s", rec, debug.Stack()))
			}
			mode = profile.mode
		}
	}()
	return profile.hook(model, apiKey)
}

// Per model wire of a profile, false when it answers none and the declared mode when its hook raises
func ProfileModelAPIMode(ctx context.Context, profile Profile, model string) (string, bool) {
	hook := profile.APIModeForModelHook
	if hook == nil && profile.PerModelMode != nil {
		hook = func(m string, _ *string) APIMode { return profile.PerModelMode(m) }
	}
	if hook == nil {
		return "", false
	}
	declared := profile.Mode
	if declared == "" {
		declared = ModeChatCompletions
	}
	mode := modelAPIMode(ctx, &perModelHookProfile{name: profile.Name, mode: declared, hook: hook}, model, nil)
	return string(mode), mode != ""
}

// Api mode for a provider that routes per model, its hard per model wire beating a stale persisted one
func (r *resolver) profileRuntimeAPIMode(provider string, apiKey, targetModel string) APIMode {
	profile := perModelProfileCtx(r.ctx, provider)
	configured := r.configuredAPIMode(provider)
	modelName := strings.TrimSpace(r.effectiveModel(targetModel))
	if configured != "" {
		if configured == ModeCodexResponses && modelName != "" && profile != nil && modelAPIMode(r.ctx, profile, modelName, nil) == ModeChatCompletions {
			return ModeChatCompletions
		}
		return configured
	}
	if profile == nil {
		return ModeChatCompletions
	}
	if modelName != "" {
		if m := modelAPIMode(r.ctx, profile, modelName, &apiKey); m != "" {
			return m
		}
	}
	return profile.mode
}

// Upgrades api mode for GPT 5, codex, and o series deployments on Azure Foundry
func azureInferredAPIMode(model string, apiMode APIMode) APIMode {
	if model == "" || apiMode == ModeAnthropicMessages {
		return apiMode
	}
	if inferred := AzureFoundryModelMode(model); inferred != "" {
		return inferred
	}
	return apiMode
}

// Persisted model.api_mode when it belongs to this provider, else URL and transport fallback
func (r *resolver) configuredOrFallbackAPIMode(provider string, baseURL, model string, opencodeByModel bool) APIMode {
	if provider == "actual" {
		if configured := r.configuredAPIMode(provider); configured != "" && configured != ModeChatCompletions {
			pkgLog().Info(fmt.Sprintf("Routing built-in Actual through chat_completions instead of persisted api_mode=%s", configured))
		}
		return ModeChatCompletions
	}
	if opencodeByModel && OpenCodeFamily(provider) != "" {
		return OpenCodeModelMode(provider, model)
	}
	if configured := r.configuredAPIMode(provider); configured != "" {
		return configured
	}
	return r.fallbackAPIMode(provider, baseURL, model)
}

// Api mode for a registry api_key provider on the explicit and env paths
func (r *resolver) apiKeyProviderAPIMode(provider string, apiKey, baseURL, model string, opencodeByModel bool) APIMode {
	if perModelProfileCtx(r.ctx, provider) != nil {
		return r.profileRuntimeAPIMode(provider, apiKey, model)
	}
	if provider == "xai" {
		return ModeCodexResponses
	}
	return r.configuredOrFallbackAPIMode(provider, baseURL, model, opencodeByModel)
}

// Opt in rewrite to codex_app_server via model.openai_runtime for openai, codex, and named custom providers
func (r *resolver) maybeApplyCodexAppServerRuntime(provider string, apiMode APIMode, requestedProvider string) APIMode {
	if settings.ModelOpenAIRuntime.Get(r.hookCtx()) != "codex_app_server" {
		return apiMode
	}
	if provider == "openai" || provider == "openai-codex" || requestedProvider == "openai" || requestedProvider == "openai-codex" ||
		(provider == "custom" && r.CodexModelProviderID(requestedProvider) != "") {
		return modeCodexAppServer
	}
	return apiMode
}

// ---- base url and credential helpers ----

const noAnthropicCredentialsMsg = "No Anthropic credentials found. Run 'icebath auth add anthropic' to sign in, " +
	"or set ANTHROPIC_TOKEN / ANTHROPIC_API_KEY."

// Resolved runtime with Actual routes pinned onto chat completions and their canonical root
func (r *resolver) runtime(provider string, apiMode APIMode, baseURL, apiKey, source string) *Runtime {
	if isActualRouteCtx(r.ctx, provider, baseURL) {
		apiMode = ModeChatCompletions
		baseURL = NormalizeActualBaseURL(baseURL)
	}
	return &Runtime{Provider: provider, APIMode: apiMode, BaseURL: baseURL, APIKey: apiKey, Source: source}
}

// model.base_url in effect for ctx only when model.provider is this provider, judged against the registry of the home bound to ctx, so a stale URL never leaks across providers
func configBaseURLForProvider(ctx context.Context, provider string) string {
	configured := strings.ToLower(strings.TrimSpace(settings.ModelProvider.Get(ctx)))
	if provider == "actual" {
		configured = NormalizeProvider(configured)
	}
	if !sameRegisteredProvider(ctx, provider, configured) {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(settings.ModelBaseURL.Get(ctx)), "/")
}

// IsForeignProviderEndpoint reports whether the base URL is another built in provider's canonical endpoint
func IsForeignProviderEndpoint(provider, baseURL string) bool {
	pconfig, ok := registryProfile(provider)
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !ok || u == "" || u == strings.TrimRight(pconfig.BaseURL, "/") {
		return false
	}
	for _, other := range Profiles() {
		if u == strings.TrimRight(other.BaseURL, "/") {
			return true
		}
	}
	return false
}

// Whether a configured model.base_url plausibly speaks the Anthropic Messages protocol
func anthropicBaseURLOverrideOK(baseURL string) bool {
	candidate := strings.TrimSpace(baseURL)
	if candidate == "" {
		return false
	}
	hostname := strings.ToLower(Hostname(candidate))
	if hostname == "" {
		return false
	}
	return hostname == "api.anthropic.com" || strings.HasSuffix(hostname, ".anthropic.com") || strings.HasSuffix(hostname, ".claude.com") ||
		strings.HasSuffix(hostname, ".azure.com") || detectAPIModeForURL(candidate) == ModeAnthropicMessages
}

// Config base URL for native Anthropic in effect for ctx, empty when absent or untrustworthy
func anthropicCfgBaseURL(ctx context.Context) string {
	cfgBaseURL := configBaseURLForProvider(ctx, "anthropic")
	if anthropicBaseURLOverrideOK(cfgBaseURL) {
		return cfgBaseURL
	}
	return ""
}

// First non empty of several env secrets
func (r *resolver) firstSecret(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.secret(n)); v != "" {
			return v
		}
	}
	return ""
}

// Anthropic token from the environment, then the login store, benched tokens skipped for the model
func (r *resolver) resolveAnthropicToken(model string) (string, func(context.Context) (string, error)) {
	modelCreds, hasModel := r.creds.(ModelCredentialSource)
	available := func(token string) string {
		if token != "" && model != "" && hasModel && modelCreds.TokenBenchedForModel("anthropic", token, model) {
			return ""
		}
		return token
	}
	if token := r.firstSecret("ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return available(token), nil
	}
	if apiKey := r.firstSecret("ANTHROPIC_API_KEY"); apiKey != "" {
		return available(apiKey), nil
	}
	if r.creds == nil {
		return "", nil
	}
	var cred Credential
	var ok bool
	if hasModel && model != "" {
		cred, ok = modelCreds.CredentialForModel("anthropic", model)
	} else {
		cred, ok = r.creds.Credential("anthropic")
	}
	if !ok {
		return "", nil
	}
	return cred.APIKey, cred.TokenSource
}

// Anthropic token or the typed error naming a model bench versus a missing login
func (r *resolver) anthropicTokenOrRaise(model string) (string, func(context.Context) (string, error), error) {
	token, src := r.resolveAnthropicToken(model)
	if token != "" || src != nil {
		return token, src, nil
	}
	if model != "" {
		if any, anySrc := r.resolveAnthropicToken(""); any != "" || anySrc != nil {
			return "", nil, newAuthErr("anthropic", fmt.Sprintf("Anthropic credentials are rate-limited for %s; "+
				"other Claude models remain available (see `icebath auth list`).", model), "", false)
		}
	}
	return "", nil, newAuthErr("anthropic", noAnthropicCredentialsMsg, "", false)
}

var nonAlnumRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// VENDOR_API_KEY from the env, vendor being the registrable hostname label
func (r *resolver) hostDerivedAPIKey(baseURL string) string {
	hostname := Hostname(baseURL)
	if hostname == "" || hostname == "localhost" || strings.Contains(hostname, ":") {
		return ""
	}
	parts := strings.Split(hostname, ".")
	for _, ch := range parts[len(parts)-1] {
		if ch >= '0' && ch <= '9' {
			return ""
		}
	}
	var labels []string
	for _, lbl := range parts {
		if lbl != "" {
			labels = append(labels, lbl)
		}
	}
	for len(labels) > 0 && (labels[0] == "api" || labels[0] == "www") {
		labels = labels[1:]
	}
	if len(labels) < 2 {
		return ""
	}
	sanitized := strings.ToUpper(nonAlnumRe.ReplaceAllString(labels[len(labels)-2], "_"))
	if sanitized == "" || !(sanitized[0] >= 'A' && sanitized[0] <= 'Z') || sanitized == "OPENAI" || sanitized == "OPENROUTER" || sanitized == "OLLAMA" {
		return ""
	}
	return strings.TrimSpace(r.secret(sanitized + "_API_KEY"))
}

// Env API keys gated on their authoritative hosts, then the host derived vendor key
func (r *resolver) hostGatedEnvKeyCandidates(baseURL string, ollama bool) []string {
	isOpenAI := HostMatches(baseURL, "openai.com") || HostMatches(baseURL, "openai.azure.com")
	envOpenAIBase := strings.TrimRight(strings.TrimSpace(r.secret("OPENAI_BASE_URL")), "/")
	isOpenAI = isOpenAI || (envOpenAIBase != "" && strings.TrimRight(strings.TrimSpace(baseURL), "/") == envOpenAIBase)
	var candidates []string
	if ollama {
		if HostMatches(baseURL, "ollama.com") {
			candidates = append(candidates, strings.TrimSpace(r.secret("OLLAMA_API_KEY")))
		} else {
			candidates = append(candidates, "")
		}
	}
	openaiKey := ""
	if isOpenAI {
		openaiKey = strings.TrimSpace(r.secret("OPENAI_API_KEY"))
	}
	openrouterKey := ""
	if HostMatches(baseURL, "openrouter.ai") {
		openrouterKey = strings.TrimSpace(r.secret("OPENROUTER_API_KEY"))
	}
	return append(candidates, openaiKey, openrouterKey, r.hostDerivedAPIKey(baseURL))
}

// Env keys a base URL host would receive, then its host derived vendor key
func HostGatedEnvKeyCandidates(ctx context.Context, h, baseURL string, ollama bool) []string {
	return modelsResolver(ctxForHome(ctx, h)).hostGatedEnvKeyCandidates(baseURL, ollama)
}

// LM Studio OpenAI compatible runtime base URL from either the native or the /v1 form
func normalizeLMStudioRuntimeBaseURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			root = strings.TrimRight(root[:len(root)-len(suffix)], "/")
			break
		}
	}
	if root == "" {
		root = "http://127.0.0.1:1234"
	}
	return root + "/v1"
}

// Whether the URL is Actual's loopback local API endpoint
func isActualLocalBaseURL(baseURL string) bool {
	return isLoopbackHostname(Hostname(baseURL))
}

func isLoopbackHostname(host string) bool {
	switch strings.TrimRight(strings.ToLower(host), ".") {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// Actual's OpenAI compatible base URL, hosted or loopback roots gaining the /v1 surface
func normalizeActualRuntimeBaseURL(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if u == "" {
		return ActualBaseURL
	}
	host := strings.TrimRight(strings.ToLower(Hostname(u)), ".")
	path := strings.TrimRight(URLPath(u), "/")
	if (path == "" || path == "/") && (host == "api.actual.inc" || isActualLocalBaseURL(u)) {
		return u + "/v1"
	}
	return u
}

// Shared tail for pool entry and api key paths, OpenCode /v1 rule then LM Studio and Actual normalization
func finalizeBaseURL(provider string, apiMode APIMode, baseURL string) string {
	if OpenCodeFamily(provider) != "" {
		baseURL = NormalizeOpenCodeBaseURL(provider, apiMode, baseURL)
	}
	if provider == "lmstudio" {
		baseURL = normalizeLMStudioRuntimeBaseURL(baseURL)
	}
	if provider == "actual" {
		baseURL = normalizeActualRuntimeBaseURL(baseURL)
	}
	return baseURL
}

// Usable secrets

var placeholderSecretValues = map[string]bool{
	"*": true, "**": true, "***": true, "changeme": true, "your_api_key": true, "your_api_key_here": true,
	"your-api-key": true, "placeholder": true, "example": true, "dummy": true, "null": true, "none": true,
}

var placeholderKeyPrefixes = []string{"sk-", "ghp_", "hf_"}

// True for the placeholder shapes shipped in env examples and docs
func isPlaceholderShape(value string) bool {
	lowered := strings.ToLower(value)
	if strings.HasPrefix(lowered, "your_") && strings.HasSuffix(lowered, "_here") {
		return true
	}
	allX := func(s string) bool {
		if s == "" {
			return false
		}
		for _, c := range s {
			if c != 'x' {
				return false
			}
		}
		return true
	}
	for _, prefix := range placeholderKeyPrefixes {
		if strings.HasPrefix(lowered, prefix) && allX(lowered[len(prefix):]) {
			return true
		}
	}
	stripped := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(lowered)
	return allX(stripped)
}

// Reports whether a configured secret looks usable rather than empty or a placeholder
func hasUsableSecret(value string) bool {
	cleaned := strings.TrimSpace(value)
	return len(cleaned) >= 4 && !placeholderSecretValues[strings.ToLower(cleaned)] && !isPlaceholderShape(cleaned)
}

// First usable Anthropic credential over the registry env vars, the home dotenv beating a stale shell export
func GetAnthropicKey(ctx context.Context) string {
	row, err := providerConfig(ctx, "anthropic")
	if err != nil {
		panic(err)
	}
	h := home.Dir(ctx)
	for _, name := range row.KeyEnvNames() {
		v, err := config.GetEnvValuePreferDotenv(ctx, h, name)
		if err != nil {
			panic(err)
		}
		if v != "" {
			return v
		}
	}
	return ""
}

var knownProviderKeyPrefixes = map[string][]string{"openrouter": {"sk-or-"}}

func matchesKeyPrefix(providerID, val string) bool {
	for _, p := range knownProviderKeyPrefixes[providerID] {
		if strings.HasPrefix(val, p) {
			return true
		}
	}
	return false
}

// True when the value carries an OpenRouter key prefix
func looksLikeOpenRouterKey(value string) bool {
	return matchesKeyPrefix("openrouter", strings.TrimSpace(value))
}

// Reports whether a credential has the sk-or- shape of an OpenRouter key
func LooksLikeOpenRouterKey(value string) bool { return looksLikeOpenRouterKey(value) }

// The value stripped when it is a usable prefix valid secret, empty after warning on a prefix mismatch
func usableDeclaredSecret(providerID, value, source string) string {
	val := strings.TrimSpace(value)
	if !hasUsableSecret(val) {
		return ""
	}
	if prefixes := knownProviderKeyPrefixes[providerID]; len(prefixes) > 0 && !matchesKeyPrefix(providerID, val) {
		pkgLog().Warn(fmt.Sprintf("Ignoring %s for provider %q: value does not match the expected key prefix (%s). "+
			"Falling back to the next credential source. Fix or remove the malformed key to silence this warning.",
			source, providerID, strings.Join(prefixes, " or ")))
		return ""
	}
	return val
}

// Api key provider credentials )

// apiKeyCreds is a resolved api key provider login with its base URL and source
type apiKeyCreds struct {
	APIKey      string
	BaseURL     string
	Source      string
	TokenSource func(ctx context.Context) (string, error)
}

// model.key_env when config.yaml's main model targets the provider
func (r *resolver) modelLevelKeyEnv(providerID string) string {
	if r.cfgProvider() != providerID {
		return ""
	}
	return strings.TrimSpace(settings.ModelKeyEnv.Get(r.hookCtx()))
}

// APIKeySecretResolver is a provider owned credential resolution returning the runtime key and its source
type APIKeySecretResolver func(ctx context.Context) (apiKey, source string, ok bool)

// RuntimeBaseURLResolver is a provider owned base URL derived from the credential, empty for env or default
type RuntimeBaseURLResolver func(ctx context.Context, apiKey, defaultURL, envURL string) string

// Api key provider secret and its source, the profile seam, model.key_env, registry env vars, then the login store
func (r *resolver) apiKeyProviderSecret(providerID string, pconfig Profile) (string, string, func(context.Context) (string, error)) {
	var owned APIKeySecretResolver
	if p := getProviderProfile(r.ctx, providerID); p != nil {
		owned = p.ResolveAPIKeySecretHook
	}
	if owned != nil {
		if key, source, ok := owned(r.hookCtx()); ok {
			return key, source, nil
		}
	}
	if keyEnv := r.modelLevelKeyEnv(providerID); keyEnv != "" {
		if val := usableDeclaredSecret(providerID, r.envPreferDotenv(keyEnv), keyEnv); val != "" {
			return val, keyEnv, nil
		}
	}
	for _, envVar := range pconfig.KeyEnvNames() {
		if val := usableDeclaredSecret(providerID, r.envPreferDotenv(envVar), envVar); val != "" {
			return val, envVar, nil
		}
	}
	if r.creds != nil {
		if cred, ok := r.creds.Credential(providerID); ok {
			if cred.TokenSource != nil {
				return "", cred.Source, cred.TokenSource
			}
			if val := usableDeclaredSecret(providerID, cred.APIKey, "credential_pool:"+providerID); val != "" {
				source := cred.Source
				if source == "" {
					source = "credential_pool:" + providerID
				}
				return val, source, nil
			}
		}
	}
	return "", "", nil
}

// Env base URL override for a registry provider, Actual honouring a configured model.base_url first
func (r *resolver) providerEnvBaseURL(pconfig Profile) string {
	if pconfig.Name == "actual" {
		if NormalizeProvider(r.cfgProvider()) == "actual" {
			if configured := r.cfgBaseURL(); configured != "" {
				return configured
			}
		}
	}
	if pconfig.BaseURLEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(pconfig.BaseURLEnv))
}

// Kimi base URL from the key prefix, an explicit env override always winning
func resolveKimiBaseURL(apiKey, defaultURL, envOverride string) string {
	if envOverride != "" {
		return envOverride
	}
	if strings.HasPrefix(apiKey, "sk-kimi-") {
		return KimiCodeBaseURL
	}
	return defaultURL
}

// Base URL for an api key provider, env override or the registry default with provider specific shaping
func (r *resolver) apiKeyProviderBaseURL(providerID, apiKey string, pconfig Profile, envURL string) string {
	defaultURL := func() string {
		if envURL != "" {
			return strings.TrimRight(envURL, "/")
		}
		return pconfig.BaseURL
	}
	switch providerID {
	case "kimi-coding", "kimi-coding-cn":
		return resolveKimiBaseURL(apiKey, pconfig.BaseURL, envURL)
	case "zai":
		return r.resolveZaiBaseURL(apiKey, pconfig.BaseURL, envURL)
	case "lmstudio":
		return normalizeLMStudioRuntimeBaseURL(defaultURL())
	case "actual":
		return normalizeActualRuntimeBaseURL(defaultURL())
	}
	return defaultURL()
}

// Api key and base URL for a registry api key provider
func (r *resolver) apiKeyProviderCredentials(providerID string) (apiKeyCreds, error) {
	pconfig, ok := registryProfileCtx(r.ctx, providerID)
	if !ok || pconfig.AuthType != AuthTypeAPIKey {
		return apiKeyCreds{}, newAuthErr(providerID, fmt.Sprintf("Provider '%s' is not an API-key provider.", providerID), "invalid_provider", false)
	}
	apiKey, keySource, tokenSrc := r.apiKeyProviderSecret(providerID, pconfig)
	if apiKey == "" && tokenSrc == nil && providerID == "lmstudio" {
		apiKey = LMStudioNoAuthPlaceholder
		if keySource == "" {
			keySource = "default"
		}
	}
	envURL := r.providerEnvBaseURL(pconfig)
	var urlSeam RuntimeBaseURLResolver
	if p := getProviderProfile(r.ctx, providerID); p != nil {
		urlSeam = p.RuntimeBaseURLHook
	}
	baseURL := ""
	if urlSeam != nil {
		baseURL = urlSeam(r.hookCtx(), apiKey, pconfig.BaseURL, envURL)
	}
	if baseURL == "" {
		baseURL = r.apiKeyProviderBaseURL(providerID, apiKey, pconfig, envURL)
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = pconfig.BaseURL
	}
	if apiKey == "" && tokenSrc == nil && providerID == "actual" && isActualLocalBaseURL(baseURL) {
		apiKey = ActualLocalNoAuthPlaceholder
		if keySource == "" {
			keySource = "local-offline"
		}
	}
	source := keySource
	if source == "" {
		source = "env"
	}
	return apiKeyCreds{APIKey: apiKey, BaseURL: baseURL, Source: source, TokenSource: tokenSrc}, nil
}

// ---- auth.resolve_provider ----

// Aliases the auth resolver accepts, the registry's own table layered under plugin aliases
var authProviderAliases = map[string]string{
	"glm": "zai", "z-ai": "zai", "z.ai": "zai", "zhipu": "zai",
	"google": "gemini", "google-gemini": "gemini", "google-ai-studio": "gemini",
	"x-ai": "xai", "x.ai": "xai", "grok": "xai",
	"xai-oauth": "xai-oauth", "x-ai-oauth": "xai-oauth", "grok-oauth": "xai-oauth", "xai-grok-oauth": "xai-oauth",
	"kimi": "kimi-coding", "kimi-for-coding": "kimi-coding", "moonshot": "kimi-coding",
	"kimi-cn": "kimi-coding-cn", "moonshot-cn": "kimi-coding-cn",
	"step": "stepfun", "stepfun-coding-plan": "stepfun",
	"arcee-ai": "arcee", "arceeai": "arcee",
	"gmi-cloud": "gmi", "gmicloud": "gmi",
	"actual-computer": "actual", "actualcomputer": "actual", "aci": "actual",
	"minimax-china": "minimax-cn", "minimax_cn": "minimax-cn",
	"minimax-portal": "minimax-oauth", "minimax-global": "minimax-oauth", "minimax_oauth": "minimax-oauth",
	"alibaba_coding": "alibaba-coding-plan", "alibaba-coding": "alibaba-coding-plan", "alibaba_coding_plan": "alibaba-coding-plan",
	"claude": "anthropic", "claude-code": "anthropic",
	"aigateway": "ai-gateway", "vercel": "ai-gateway", "vercel-ai-gateway": "ai-gateway",
	"opencode": "opencode-zen", "zen": "opencode-zen",
	"qwen-portal": "qwen-oauth", "qwen-cli": "qwen-oauth", "qwen-oauth": "qwen-oauth",
	"hf": "huggingface", "hugging-face": "huggingface", "huggingface-hub": "huggingface",
	"mimo": "xiaomi", "xiaomi-mimo": "xiaomi",
	"tencent": "tencent-tokenhub", "tokenhub": "tencent-tokenhub", "tencent-cloud": "tencent-tokenhub", "tencentmaas": "tencent-tokenhub",
	"tokenplan": "tencent-tokenplan", "tencent-lkeap": "tencent-tokenplan",
	"aws": "bedrock", "aws-bedrock": "bedrock", "amazon-bedrock": "bedrock", "amazon": "bedrock",
	"go": "opencode-go", "opencode-go-sub": "opencode-go",
	"kilo": "kilocode", "kilo-code": "kilocode", "kilo-gateway": "kilocode",
	"lmstudio": "lmstudio", "lm-studio": "lmstudio", "lm_studio": "lmstudio",
	"chatgpt": "openai-codex", "chatgpt-codex": "openai-codex",
	"local": "custom", "ollama": "custom", "ollama_cloud": "ollama-cloud",
	"vllm": "custom", "llamacpp": "custom", "llama.cpp": "custom", "llama-cpp": "custom",
}

// Auth style alias collapse, the registry table first then plugin declared aliases
func authNormalizeProvider(name string) string {
	return authNormalizeProviderCtx(context.Background(), name)
}

// Auth style alias collapse against the registry of the home bound to ctx
func authNormalizeProviderCtx(ctx context.Context, name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if canon, ok := authProviderAliases[key]; ok {
		return canon
	}
	if canon := normalizeProviderCtx(ctx, key); canon != key && canon != "openrouter" {
		if _, ok := profileForCtx(ctx, canon); ok {
			return canon
		}
	}
	return key
}

// True when a provider alias resolves to custom
func (r *resolver) resolvesToCustom(name string) bool {
	resolved, err := r.resolveProvider(name, "", "")
	return err == nil && resolved == "custom"
}

// Whether model.base_url may back bare custom runtime resolution
func (r *resolver) configBaseURLTrustworthyForBareCustom(cfgBaseURL, cfgProvider string) bool {
	norm := strings.ToLower(strings.TrimSpace(cfgProvider))
	bu := strings.TrimSpace(cfgBaseURL)
	if bu == "" {
		return false
	}
	if norm == "custom" {
		return true
	}
	if norm != "" && norm != "auto" && r.resolvesToCustom(norm) {
		return true
	}
	return !HostMatches(bu, "openrouter.ai") && isLoopbackHostname(Hostname(bu))
}

// Refuses env key auto adoption of openrouter while config.yaml is corrupt
func (r *resolver) refuseEnvAdoptionIfConfigCorrupt() error {
	failure, active := config.ActiveConfigParseFailure(r.home)
	if !active || failure == "" {
		return nil
	}
	return newAuthErr("", fmt.Sprintf("config.yaml at %s is corrupt (%s) — refusing to auto-select "+
		"an inference provider from environment keys. Fix the YAML (a backup "+
		"was saved next to it), or start icebath and type /setup model.", config.GetConfigPath(r.home), failure), "corrupt_config", false)
}

// True when an OpenRouter credential exists via env key or the login store
func (r *resolver) openrouterAutoDetected() bool {
	if hasUsableSecret(r.secret("OPENROUTER_API_KEY")) {
		return true
	}
	legacy := r.secret("OPENAI_API_KEY")
	if hasUsableSecret(legacy) && looksLikeOpenRouterKey(legacy) {
		return true
	}
	if r.creds == nil {
		return false
	}
	if src, ok := r.creds.(PoolPresenceSource); ok {
		return src.HasPoolCredentials("openrouter")
	}
	_, ok := r.creds.Credential("openrouter")
	return ok
}

// Registry provider whose stored logins report signed in, empty when the credential source names none
func (r *resolver) signedInProvider() string {
	src, ok := r.creds.(SignedInSource)
	if !ok {
		return ""
	}
	signedIn := src.SignedInProvider()
	if _, known := registryProfileCtx(r.ctx, signedIn); !known {
		return ""
	}
	return signedIn
}

// Provider model.provider names when it pins a registry provider or a custom endpoint
func (r *resolver) configModelProvider() string {
	provider := authNormalizeProvider(r.cfgProvider())
	if provider == "custom" || strings.HasPrefix(provider, "custom:") {
		return "custom"
	}
	if _, ok := registryProfileCtx(r.ctx, provider); provider == "openrouter" || ok {
		return provider
	}
	if provider != "" && r.HasNamedCustomProvider(provider) {
		return "custom"
	}
	if baseURL := r.cfgBaseURL(); baseURL != "" && r.configBaseURLTrustworthyForBareCustom(baseURL, provider) {
		return "custom"
	}
	return ""
}

var noAutoDetectProviders = map[string]bool{"lmstudio": true}

// First registry api key provider in registry order with a usable env key
func (r *resolver) envKeyAutoDetected(signedIn string) string {
	ambient := map[string]bool{}
	for _, p := range listProviders(r.ctx) {
		if p.AmbientCredentials {
			ambient[p.Name] = true
		}
	}
	for _, pconfig := range canonicalRegistryRows(r.ctx) {
		pid := pconfig.Name
		if pconfig.AuthType != AuthTypeAPIKey || noAutoDetectProviders[pid] || ambient[pid] {
			continue
		}
		for _, envVar := range pconfig.KeyEnvNames() {
			if hasUsableSecret(r.secret(envVar)) {
				if signedIn != "" && signedIn != pid {
					pkgLog().Warn(fmt.Sprintf("Provider resolved to %q via %s, preempting your signed-in provider %q. "+
						"If you meant to use that login, unset %s or set `model.provider` explicitly.", pid, envVar, signedIn, envVar))
				}
				return pid
			}
		}
	}
	return ""
}

// Hint for an unknown provider error, a moved provider's install line, a removed provider, or config issues
func (r *resolver) unknownProviderHint(providerName string) string {
	if moved := ProviderInstallHint(home.WithOverride(r.ctx, r.home), providerName); moved != "" {
		return moved
	}
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "opencode-free", "free", "opencode_free":
		return "OpenCode discontinued anonymous free-tier access outside its own client " +
			"(relay 403s FreeTierError), so the keyless 'opencode-free' provider was removed. " +
			"Switch to 'opencode-zen' (pay-as-you-go, OPENCODE_ZEN_API_KEY) or 'opencode-go' " +
			"($10/mo subscription, OPENCODE_GO_API_KEY) via 'icebath model'."
	}
	issues := configStructureIssueLines(r.hookCtx())
	if len(issues) == 0 {
		return ""
	}
	lines := append([]string{"Config issue detected — run 'icebath doctor' for full diagnostics:"}, issues...)
	return strings.Join(lines, "\n")
}

// Which inference provider to use, explicit intent beating a stale OAuth login on the auto path
func (r *resolver) resolveProvider(requested, explicitAPIKey, explicitBaseURL string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(requested))
	if normalized == "" {
		normalized = "auto"
	}
	normalized = authNormalizeProviderCtx(r.ctx, normalized)
	if normalized == "openrouter" || normalized == "custom" {
		return normalized, nil
	}
	if _, ok := registryProfileCtx(r.ctx, normalized); ok {
		return normalized, nil
	}
	if normalized != "auto" {
		hint := r.unknownProviderHint(normalized)
		tail := " Check 'icebath model' for available providers, or run 'icebath doctor' to diagnose config issues."
		if hint != "" {
			tail = "\n\n" + hint
		}
		return "", newAuthErr("", fmt.Sprintf("Unknown provider '%s'.", normalized)+tail, "invalid_provider", false)
	}
	if explicitAPIKey != "" || explicitBaseURL != "" {
		return "openrouter", nil
	}
	cfgProvider := r.configModelProvider()
	if cfgProvider != "" {
		return cfgProvider, nil
	}
	if r.openrouterAutoDetected() {
		if err := r.refuseEnvAdoptionIfConfigCorrupt(); err != nil {
			return "", err
		}
		return "openrouter", nil
	}
	signedIn := r.signedInProvider()
	if envPid := r.envKeyAutoDetected(signedIn); envPid != "" {
		return envPid, nil
	}
	if signedIn != "" {
		if r.cfgProvider() == "" && r.cfgModel() != "" {
			pkgLog().Warn(fmt.Sprintf("Provider resolved to the signed-in provider %q because config.yaml `model` has no `provider` key. "+
				"If you meant a different provider, set `model.provider` explicitly.", signedIn))
		}
		return signedIn, nil
	}
	if hasAWSCredentials(r.ctx, true) {
		return "bedrock", nil
	}
	return "", newAuthErr("", fmt.Sprintf("Icebath is not connected to any AI provider yet. Run `icebath model` to pick one, type `/setup model` in chat, "+
		"or add a key with `icebath auth add <provider>`. (Advanced: put an API key such as OPENROUTER_API_KEY in %s/.env.)",
		displayHome(r.home)), "no_provider_configured", false)
}

// User facing home path with the account home folded to a tilde
func displayHome(h string) string {
	if userHome, err := os.UserHomeDir(); err == nil && userHome != "" && strings.HasPrefix(h, userHome) {
		return "~" + strings.TrimPrefix(h, userHome)
	}
	return h
}

// ---- credential pool entries ----

type poolSimpleMode struct {
	mode       APIMode
	defaultURL func() string
}

// Pool entry providers whose api mode is fixed, with the default base URL when the row carries none
var poolEntrySimpleModes = map[string]poolSimpleMode{
	"openai-codex": {ModeCodexResponses, func() string { return CodexBaseURL }},
	"xai-oauth":    {ModeCodexResponses, func() string { return DefaultXAIOAuthBaseURL }},
	"qwen-oauth":   {ModeChatCompletions, func() string { return qwenBaseURLFromEnv() }},
	"openrouter":   {ModeChatCompletions, func() string { return OpenRouterBaseURL }},
	"minimax-oauth": {ModeAnthropicMessages, func() string {
		p, _ := ProfileFor("minimax-oauth")
		return p.BaseURL
	}},
	"xai": {ModeCodexResponses, func() string { return "" }},
}

var v1TailRe = regexp.MustCompile(`/v1/?$`)

// Api mode and base URL for a pool entry of the provider
func (r *resolver) poolEntryModeAndURL(provider string, cred Credential, model, baseURL string) (APIMode, string) {
	if provider == "actual" && strings.HasPrefix(strings.TrimPrefix(cred.Source, "pool:"), "env:") {
		if cfgURL := configBaseURLForProvider(r.hookCtx(), provider); cfgURL != "" {
			baseURL = cfgURL
		}
	}
	if simple, ok := poolEntrySimpleModes[provider]; ok {
		defaultURL := simple.defaultURL()
		if provider == "openai-codex" {
			if override := strings.TrimRight(strings.TrimSpace(r.secret("ICEBATH_CODEX_BASE_URL")), "/"); override != "" {
				return simple.mode, override
			}
			if baseURL == "" || baseURL == defaultURL {
				if cfgURL := configBaseURLForProvider(r.hookCtx(), provider); cfgURL != "" {
					baseURL = cfgURL
				}
			}
		}
		if provider == "xai" {
			canonical := ""
			if p, ok := ProfileFor("xai"); ok {
				canonical = strings.TrimRight(p.BaseURL, "/")
			}
			if t := strings.TrimRight(baseURL, "/"); t == "" || t == canonical {
				if cfgURL := configBaseURLForProvider(r.hookCtx(), provider); cfgURL != "" {
					baseURL = cfgURL
				}
			}
		}
		if baseURL == "" {
			baseURL = defaultURL
		}
		return simple.mode, baseURL
	}
	if provider == "anthropic" {
		u := anthropicCfgBaseURL(r.hookCtx())
		if u == "" {
			u = baseURL
		}
		if u == "" {
			u = AnthropicBaseURL
		}
		return ModeAnthropicMessages, u
	}
	if perModelProfileCtx(r.ctx, provider) != nil {
		mode := r.profileRuntimeAPIMode(provider, cred.APIKey, model)
		if baseURL == "" {
			if p, ok := ProfileFor(provider); ok {
				baseURL = p.BaseURL
			}
		}
		return mode, baseURL
	}
	if provider == "azure-foundry" {
		mode := ModeChatCompletions
		if r.cfgProvider() == "azure-foundry" {
			if cfgURL := configBaseURLForProvider(r.hookCtx(), "azure-foundry"); cfgURL != "" {
				baseURL = cfgURL
			}
			if parsed := r.cfgAPIMode(); parsed != "" {
				mode = parsed
			}
		}
		mode = azureInferredAPIMode(model, mode)
		if mode == ModeAnthropicMessages {
			return mode, v1TailRe.ReplaceAllString(baseURL, "")
		}
		return mode, baseURL
	}
	if pconfig, ok := registryProfileCtx(r.ctx, provider); ok && (baseURL == "" || strings.TrimRight(baseURL, "/") == strings.TrimRight(pconfig.BaseURL, "/")) {
		if cfgURL := configBaseURLForProvider(r.hookCtx(), provider); cfgURL != "" {
			baseURL = cfgURL
		} else if baseURL == "" {
			baseURL = pconfig.BaseURL
		}
	}
	return r.configuredOrFallbackAPIMode(provider, baseURL, model, true), baseURL
}

// Runtime from a selected login of the provider
func (r *resolver) runtimeFromPoolEntry(provider string, cred Credential, requestedProvider string, targetModel string) *Runtime {
	mode, baseURL := r.poolEntryModeAndURL(provider, cred, r.effectiveModel(targetModel), strings.TrimRight(cred.BaseURL, "/"))
	baseURL = finalizeBaseURL(provider, mode, baseURL)
	source := cred.Source
	if source == "" {
		source = "pool"
	}
	rt := r.runtime(provider, mode, baseURL, cred.APIKey, source)
	rt.TokenSource = cred.TokenSource
	rt.RequestedProvider = requestedProvider
	rt.CredentialPool = cred.Pool
	return rt
}

// OpenRouter pool only for a plain openrouter or auto request with no custom endpoint or override
func (r *resolver) openrouterShouldUsePool(requestedProvider string, explicitAPIKey, explicitBaseURL string) bool {
	cfgBaseURL := r.cfgBaseURL()
	envBase := strings.TrimSpace(r.secret("OPENAI_BASE_URL"))
	if envBase == "" {
		envBase = strings.TrimSpace(r.secret("OPENROUTER_BASE_URL"))
	}
	cfgProv := r.cfgProvider()
	cfgIsMirror := cfgBaseURL != "" && (cfgProv == "auto" || cfgProv == "custom" || (cfgProv == "openrouter" && !HostMatches(cfgBaseURL, "openrouter.ai")))
	hasCustomEndpoint := explicitBaseURL != "" || envBase != "" || cfgIsMirror
	return (requestedProvider == "openrouter" || requestedProvider == "auto") && !hasCustomEndpoint && explicitAPIKey == "" && explicitBaseURL == ""
}

// Login selected for the provider and model when the source can judge model benches
func (r *resolver) credentialForModel(provider, model string) (Credential, bool) {
	if r.creds == nil {
		return Credential{}, false
	}
	if mc, ok := r.creds.(ModelCredentialSource); ok && model != "" {
		return mc.CredentialForModel(provider, model)
	}
	return r.creds.Credential(provider)
}

// Runtime from the provider's credential pool, nil to continue down the ladder
func (r *resolver) resolveFromPool(provider, requestedProvider string, explicitAPIKey, explicitBaseURL, targetModel string) *Runtime {
	if provider == "openrouter" && !r.openrouterShouldUsePool(requestedProvider, explicitAPIKey, explicitBaseURL) {
		return nil
	}
	cred, ok := r.credentialForModel(provider, targetModel)
	if !ok {
		return nil
	}
	if p := getProviderProfile(r.ctx, provider); p != nil && p.PreparePoolEntryHook != nil {
		entry := PooledCredential{Provider: provider, AuthType: AuthTypeAPIKey, AccessToken: cred.APIKey, BaseURL: cred.BaseURL}
		cred.APIKey = p.PreparePoolEntry(r.hookCtx(), &entry, cred.APIKey)
		cred.BaseURL = entry.RuntimeBaseURL()
	}
	if cred.TokenSource == nil && !hasUsableSecret(cred.APIKey) {
		return nil
	}
	return r.runtimeFromPoolEntry(provider, cred, requestedProvider, targetModel)
}

// Refusal of a provider whose selected login or rotation set serves nothing, so the ladder never falls over to another account
func (r *resolver) selectedLoginUnavailable(provider, requestedProvider string, explicitAPIKey, explicitBaseURL, targetModel string) *AuthError {
	if provider == "openrouter" && !r.openrouterShouldUsePool(requestedProvider, explicitAPIKey, explicitBaseURL) {
		return nil
	}
	src, ok := r.creds.(interface {
		LoginUnavailable(provider, model string) *AuthError
	})
	if !ok {
		return nil
	}
	return src.LoginUnavailable(provider, targetModel)
}

// ---- explicit --api-key / --base-url path ----

func (r *resolver) explicitAnthropic(requestedProvider string, apiKey, baseURL, targetModel string) (*Runtime, error) {
	if baseURL == "" {
		baseURL = anthropicCfgBaseURL(r.hookCtx())
	}
	if baseURL == "" {
		baseURL = AnthropicBaseURL
	}
	var src func(context.Context) (string, error)
	if apiKey == "" {
		var err error
		apiKey, src, err = r.anthropicTokenOrRaise(targetModel)
		if err != nil {
			return nil, err
		}
	}
	rt := r.runtime("anthropic", ModeAnthropicMessages, baseURL, apiKey, "explicit")
	rt.TokenSource = src
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Stored login of an oauth provider or its typed missing credential error
func (r *resolver) oauthCredential(provider string) (Credential, error) {
	if src, ok := r.creds.(RuntimeCredentialSource); ok {
		return src.RuntimeCredential(provider)
	}
	if r.creds != nil {
		if cred, ok := r.creds.Credential(provider); ok && (cred.APIKey != "" || cred.TokenSource != nil) {
			return cred, nil
		}
	}
	return Credential{}, oauthMissingLoginError(provider)
}

// Typed error for an oauth provider with no stored login
func oauthMissingLoginError(provider string) *AuthError {
	switch provider {
	case "openai-codex":
		return codexErr("No Codex credentials stored. Run `icebath auth add openai-codex` to authenticate.", "codex_auth_missing", true)
	case "xai-oauth":
		return xaiErr("No xAI OAuth credentials stored. Select xAI Grok OAuth (SuperGrok / Premium+) in `icebath model`.", "xai_auth_missing", true)
	case "qwen-oauth":
		return qwenErr("Qwen OAuth access token missing. Re-run 'icebath auth qwen-oauth'.", "qwen_access_token_missing", true)
	case "minimax-oauth":
		return minimaxErr("MiniMax OAuth credentials not found. Run `icebath auth add minimax-oauth` to sign in.", "minimax_auth_missing", true)
	}
	return newAuthErr(provider, fmt.Sprintf("No usable credentials found for provider '%s'.", provider), "missing_api_key", true)
}

// Takes key, base URL, and token source from the stored login when no explicit key was given
func (r *resolver) credsFallback(provider, apiKey, explicitBaseURL, baseURL string) (string, string, func(context.Context) (string, error), string, error) {
	if apiKey != "" {
		return apiKey, baseURL, nil, "explicit", nil
	}
	cred, err := r.oauthCredential(provider)
	if err != nil {
		return "", "", nil, "", err
	}
	u := explicitBaseURL
	if u == "" {
		u = strings.TrimRight(cred.BaseURL, "/")
	}
	if u == "" {
		u = baseURL
	}
	return cred.APIKey, u, cred.TokenSource, "explicit", nil
}

func (r *resolver) explicitCodex(requestedProvider string, apiKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	base := explicitBaseURL
	if base == "" {
		base = CodexBaseURL
	}
	key, baseURL, src, source, err := r.credsFallback("openai-codex", apiKey, explicitBaseURL, base)
	if err != nil {
		return nil, err
	}
	rt := r.runtime("openai-codex", ModeCodexResponses, baseURL, key, source)
	rt.TokenSource = src
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Qwen portal base URL with the ICEBATH_QWEN_BASE_URL override applied
func qwenBaseURLFromEnv() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("ICEBATH_QWEN_BASE_URL")), "/"); v != "" {
		return v
	}
	return QwenBaseURL
}

func (r *resolver) explicitQwen(requestedProvider string, apiKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	base := explicitBaseURL
	if base == "" {
		base = qwenBaseURLFromEnv()
	}
	key, baseURL, src, source, err := r.credsFallback("qwen-oauth", apiKey, explicitBaseURL, base)
	if err != nil {
		return nil, err
	}
	rt := r.runtime("qwen-oauth", ModeChatCompletions, baseURL, key, source)
	rt.TokenSource = src
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Actual's loopback daemon speaks a no auth local API, substitute the placeholder key
func actualLocalKey(provider, apiKey, baseURL string) string {
	if provider == "actual" && apiKey == "" && isActualLocalBaseURL(baseURL) {
		return ActualLocalNoAuthPlaceholder
	}
	return apiKey
}

func actualURL(provider, baseURL string) string {
	if provider == "actual" {
		return normalizeActualRuntimeBaseURL(baseURL)
	}
	return baseURL
}

func (r *resolver) explicitAPIKeyProvider(provider string, pconfig Profile, requestedProvider string, apiKey, baseURL, targetModel string) (*Runtime, error) {
	var tokenSrc func(context.Context) (string, error)
	if baseURL == "" {
		switch {
		case provider == "actual":
			baseURL = configBaseURLForProvider(r.hookCtx(), provider)
			if baseURL == "" {
				creds, err := r.apiKeyProviderCredentials(provider)
				if err != nil {
					return nil, err
				}
				baseURL = creds.BaseURL
			}
		case provider == "kimi-coding" || provider == "kimi-coding-cn":
			creds, err := r.apiKeyProviderCredentials(provider)
			if err != nil {
				return nil, err
			}
			baseURL = strings.TrimRight(creds.BaseURL, "/")
		default:
			envURL := ""
			if pconfig.BaseURLEnv != "" {
				envURL = strings.TrimRight(strings.TrimSpace(r.secret(pconfig.BaseURLEnv)), "/")
			}
			baseURL = envURL
			if baseURL == "" {
				baseURL = pconfig.BaseURL
			}
		}
	}
	baseURL = actualURL(provider, baseURL)
	if apiKey == "" {
		creds, err := r.apiKeyProviderCredentials(provider)
		if err != nil {
			return nil, err
		}
		apiKey = creds.APIKey
		tokenSrc = creds.TokenSource
		if baseURL == "" {
			baseURL = actualURL(provider, strings.TrimRight(creds.BaseURL, "/"))
		}
	}
	model := targetModel
	if model == "" {
		model = r.cfgModel()
	}
	mode := r.apiKeyProviderAPIMode(provider, apiKey, baseURL, model, true)
	baseURL = finalizeBaseURL(provider, mode, baseURL)
	apiKey = actualLocalKey(provider, apiKey, baseURL)
	rt := r.runtime(provider, mode, strings.TrimRight(baseURL, "/"), apiKey, "explicit")
	rt.TokenSource = tokenSrc
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Runtime from explicit CLI credentials, nil when none were given or the provider takes none
func (r *resolver) resolveExplicitRuntime(provider, requestedProvider string, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	explicitAPIKey = strings.TrimSpace(explicitAPIKey)
	explicitBaseURL = strings.TrimRight(strings.TrimSpace(explicitBaseURL), "/")
	if explicitAPIKey == "" && explicitBaseURL == "" {
		return nil, nil
	}
	switch provider {
	case "anthropic":
		return r.explicitAnthropic(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
	case "openai-codex":
		return r.explicitCodex(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
	case "qwen-oauth":
		return r.explicitQwen(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
	case "azure-foundry":
		return r.resolveAzureFoundryRuntime(requestedProvider, explicitAPIKey, nil, explicitBaseURL, targetModel)
	}
	pconfig, ok := registryProfileCtx(r.ctx, provider)
	if !ok || pconfig.AuthType != AuthTypeAPIKey {
		return nil, nil
	}
	return r.explicitAPIKeyProvider(provider, pconfig, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
}

// ---- OAuth and auth store providers ----

type oauthRuntimeSpec struct {
	mode           func(model string, r *resolver) APIMode
	defaultSource  string
	failureMsg     string
	defaultBaseURL string
}

// Env and auth store OAuth providers resolved by a single credential call
var oauthRuntimeProviders = map[string]oauthRuntimeSpec{
	"openai-codex": {func(string, *resolver) APIMode { return ModeCodexResponses }, "icebath-auth-store",
		"Auto-detected Codex provider but credentials failed", ""},
	"xai-oauth": {func(string, *resolver) APIMode { return ModeCodexResponses }, "icebath-auth-store",
		"Auto-detected xAI OAuth provider but credentials failed", DefaultXAIOAuthBaseURL},
	"qwen-oauth": {func(string, *resolver) APIMode { return ModeChatCompletions }, "qwen-cli",
		"Qwen OAuth credentials failed", ""},
}

// Runtime from an oauth spec, raising when the stored login is stale, revoked, or benched
func (r *resolver) resolveOAuthRuntime(provider, requestedProvider string, targetModel string) (*Runtime, error) {
	spec := oauthRuntimeProviders[provider]
	cred, err := r.oauthCredential(provider)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = spec.defaultBaseURL
	}
	source := cred.Source
	if source == "" {
		source = spec.defaultSource
	}
	rt := r.runtime(provider, spec.mode(r.effectiveModel(targetModel), r), baseURL, cred.APIKey, source)
	rt.TokenSource = cred.TokenSource
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

func (r *resolver) minimaxOAuthRuntime(provider, requestedProvider string) (*Runtime, error) {
	pconfig, ok := registryProfileCtx(r.ctx, provider)
	if !ok || pconfig.AuthType != AuthTypeOAuthMiniMax {
		return nil, nil
	}
	cred, err := r.oauthCredential(provider)
	if err != nil {
		return nil, err
	}
	source := cred.Source
	if source == "" {
		source = "oauth"
	}
	rt := r.runtime(provider, ModeAnthropicMessages, strings.TrimRight(cred.BaseURL, "/"), cred.APIKey, source)
	rt.TokenSource = cred.TokenSource
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// ---- env and config paths for anthropic and registry api_key providers ----

// Azure Anthropic key from model.key_env and an inline model.api_key when fromModel, then the fixed env names
func (r *resolver) azureAnthropicEnvKey(fromModel bool) string {
	if fromModel {
		if envVar := strings.TrimSpace(settings.ModelKeyEnv.Get(r.hookCtx())); envVar != "" {
			if token := strings.TrimSpace(r.secret(envVar)); token != "" {
				return token
			}
		}
		if inline := strings.TrimSpace(settings.ModelAPIKey.Get(r.hookCtx())); inline != "" {
			return inline
		}
	}
	return r.firstSecret("AZURE_ANTHROPIC_KEY", "ANTHROPIC_API_KEY")
}

// Native Anthropic from env and the login store, model.base_url honoured only for provider anthropic
func (r *resolver) anthropicEnvRuntime(requestedProvider string, targetModel string) (*Runtime, error) {
	baseURL := anthropicCfgBaseURL(r.hookCtx())
	if baseURL == "" {
		baseURL = AnthropicBaseURL
	}
	var token string
	var src func(context.Context) (string, error)
	if HostMatches(baseURL, "azure.com") {
		token = r.azureAnthropicEnvKey(true)
		if token == "" {
			return nil, newAuthErr("anthropic", "No Azure Anthropic API key found. Set AZURE_ANTHROPIC_KEY or ANTHROPIC_API_KEY, or point "+
				"key_env in your config.yaml model section at a custom env var.", "", false)
		}
	} else {
		var err error
		token, src, err = r.anthropicTokenOrRaise(targetModel)
		if err != nil {
			return nil, err
		}
	}
	rt := r.runtime("anthropic", ModeAnthropicMessages, baseURL, token, "env")
	rt.TokenSource = src
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Registry api_key providers from env and config, an empty key failing closed with the env names to set
func (r *resolver) apiKeyProviderRuntime(provider string, pconfig Profile, requestedProvider string, targetModel string) (*Runtime, error) {
	creds, err := r.apiKeyProviderCredentials(provider)
	if err != nil {
		return nil, err
	}
	if provider == "actual" && creds.TokenSource == nil && !hasUsableSecret(creds.APIKey) {
		cfgURL := configBaseURLForProvider(r.hookCtx(), provider)
		probe := cfgURL
		if probe == "" {
			probe = strings.TrimRight(creds.BaseURL, "/")
		}
		if isActualLocalBaseURL(normalizeActualRuntimeBaseURL(probe)) {
			creds.APIKey = ActualLocalNoAuthPlaceholder
			if creds.Source == "" || creds.Source == "env" {
				creds.Source = "local-offline"
			}
		}
	}
	if creds.TokenSource == nil && !hasUsableSecret(creds.APIKey) {
		hint := ""
		if names := pconfig.KeyEnvNames(); len(names) > 0 {
			hint = fmt.Sprintf(" Set %s.", strings.Join(names, ", "))
		}
		hint += missingCredentialsSuffix(r.ctx, provider)
		hint += r.namedProfileKeyHint(pconfig.KeyEnvNames())
		return nil, newAuthErr(provider, fmt.Sprintf("No usable credentials found for provider '%s'.%s", provider, hint), "missing_api_key", false)
	}
	baseURL := configBaseURLForProvider(r.hookCtx(), provider)
	if baseURL == "" {
		baseURL = strings.TrimRight(creds.BaseURL, "/")
	}
	baseURL = actualURL(provider, baseURL)
	model := targetModel
	if model == "" {
		model = r.cfgModel()
	}
	mode := r.apiKeyProviderAPIMode(provider, creds.APIKey, baseURL, model, true)
	baseURL = finalizeBaseURL(provider, mode, baseURL)
	apiKey := actualLocalKey(provider, creds.APIKey, baseURL)
	rt := r.runtime(provider, mode, baseURL, apiKey, creds.Source)
	rt.TokenSource = creds.TokenSource
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

var (
	missingHintsMu            sync.Mutex
	missingHintFailuresLogged = map[string]bool{}
)

// A space led hint from the bound home profile for a missing credentials message, else empty
func missingCredentialsSuffix(ctx context.Context, provider string) (out string) {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return ""
	}
	p := getProviderProfile(ctx, name)
	if p == nil || p.MissingCredentialsHintHook == nil {
		return ""
	}
	defer func() {
		if rec := recover(); rec != nil {
			missingHintsMu.Lock()
			if !missingHintFailuresLogged[name] {
				missingHintFailuresLogged[name] = true
				pkgLog().Warn(name + " missing_credentials_hint failed; showing the error without it")
			}
			missingHintsMu.Unlock()
			out = ""
		}
	}()
	hint, ok := p.MissingCredentialsHintHook().(string)
	if !ok || strings.TrimSpace(hint) == "" {
		return ""
	}
	return " " + strings.TrimSpace(hint)
}

// ---- the resolution ladder ----

var vertexNames = map[string]bool{"vertex": true, "google-vertex": true, "vertex-ai": true, "gcp-vertex": true, "vertexai": true}

// Honours providers.<name>.enabled false for built ins too
func (r *resolver) raiseIfProviderDisabled(requestedProvider string) error {
	block, ok := r.providers()[requestedProvider].(map[string]any)
	if ok && !IsProviderEnabled(block) {
		return &ValueError{Message: fmt.Sprintf("provider %s is disabled in config (providers.%s.enabled: false)", pyrepr.StrRepr(requestedProvider), requestedProvider)}
	}
	return nil
}

// A local server alias with no endpoint configured anywhere never walks down to OpenRouter
func (r *resolver) raiseIfLocalAliasMissingEndpoint(requestedProvider, explicitBaseURL string) error {
	norm := strings.ToLower(strings.TrimSpace(requestedProvider))
	if norm == "" || norm == "custom" || IsLlamaCppAlias(norm) || !r.resolvesToCustom(norm) {
		return nil
	}
	if strings.TrimSpace(explicitBaseURL) != "" || strings.TrimSpace(r.secret("CUSTOM_BASE_URL")) != "" {
		return nil
	}
	if r.configBaseURLTrustworthyForBareCustom(r.cfgBaseURL(), r.cfgProvider()) {
		return nil
	}
	if entry := r.getNamedCustomProvider(requestedProvider, r.providers(), true); entry != nil && strings.TrimSpace(cfgString(entry["base_url"])) != "" {
		return nil
	}
	return newAuthErr(requestedProvider, fmt.Sprintf("provider '%s' has no endpoint configured, so the request is not sent anywhere "+
		"(it would otherwise fall back to OpenRouter). Set providers.%s.base_url or model.base_url in config.yaml.",
		requestedProvider, norm), "missing_base_url", false)
}

// Vertex AI with a short lived token minted per call rather than a static key
func (r *resolver) resolveVertexRuntime(requestedProvider string) (*Runtime, error) {
	tokenSrc, baseURL, ok := vertexRuntimeConfig(r.ctx, r.secrets, r.home)
	if !ok || tokenSrc == nil || baseURL == "" {
		return nil, newAuthErr("vertex", "Vertex AI credentials could not be resolved. Vertex uses OAuth2 (not a static API key): provide a "+
			"service-account JSON via GOOGLE_APPLICATION_CREDENTIALS (or VERTEX_CREDENTIALS_PATH) in ~/.icebath/.env, "+
			"or run 'gcloud auth application-default login' for ADC. Set the GCP project/region under vertex: in "+
			"config.yaml if they aren't embedded in the credentials. Start `icebath` and type `/setup model` to configure Vertex.", "", false)
	}
	rt := r.runtime("vertex", ModeChatCompletions, strings.TrimRight(baseURL, "/"), "", "vertex-oauth")
	rt.TokenSource = tokenSrc
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Providers decided on the requested name alone, before custom, pool, and generic paths
func (r *resolver) resolveRequestedShortcuts(requestedProvider, explicitAPIKey, explicitBaseURL string, explicitToken func(context.Context) (string, error), targetModel string) (*Runtime, error) {
	if requestedProvider == "moa" {
		rt := r.runtime("moa", ModeChatCompletions, MoaBaseURL, MoaAPIKey, "moa-virtual-provider")
		rt.RequestedProvider = requestedProvider
		return rt, nil
	}
	effBase := strings.TrimSpace(explicitBaseURL)
	if requestedProvider == "anthropic" && HostMatches(effBase, "azure.com") {
		key := strings.TrimSpace(explicitAPIKey)
		if key == "" {
			key = r.azureAnthropicEnvKey(false)
		}
		rt := r.runtime("anthropic", ModeAnthropicMessages, strings.TrimRight(effBase, "/"), key, "azure-explicit")
		rt.RequestedProvider = requestedProvider
		return rt, nil
	}
	if requestedProvider == "azure-foundry" {
		return r.resolveAzureFoundryRuntime(requestedProvider, explicitAPIKey, explicitToken, explicitBaseURL, targetModel)
	}
	if vertexNames[requestedProvider] {
		return r.resolveVertexRuntime(requestedProvider)
	}
	return nil, nil
}

// Rung 3, a configured named custom provider honouring the codex app server opt in
func (r *resolver) namedCustomRung(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	rt, err := r.resolveNamedCustomRuntime(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
	if err != nil || rt == nil {
		return rt, err
	}
	rt.RequestedProvider = requestedProvider
	if rt.Provider == "custom" {
		mode := rt.APIMode
		if mode == "" {
			mode = ModeChatCompletions
		}
		rt.APIMode = r.maybeApplyCodexAppServerRuntime("custom", mode, requestedProvider)
	}
	return rt, nil
}

func (r *resolver) openrouterFallback(requestedProvider, explicitAPIKey, explicitBaseURL string) *Runtime {
	rt := r.resolveOpenRouterRuntime(requestedProvider, explicitAPIKey, explicitBaseURL)
	rt.RequestedProvider = requestedProvider
	return rt
}

// Rejects a bare custom request that fell through the whole ladder to OpenRouter with no credential
func raiseForCredentiallessBareCustom(requestedProvider string, rt *Runtime) error {
	if requestedProvider != "custom" || rt.Provider != "custom" || rt.APIKey != "" || rt.TokenSource != nil {
		return nil
	}
	return newAuthErr(requestedProvider, fmt.Sprintf("provider '%s' resolved without credentials (no endpoint or API key configured). "+
		"If this is a named custom provider, use its real name (see providers: in config.yaml).", requestedProvider), "missing_api_key", false)
}

// Rungs 2 to 8, each evaluated only when the previous one returned nothing
func (r *resolver) ladderRungs(requestedProvider, explicitAPIKey, explicitBaseURL string, explicitToken func(context.Context) (string, error), targetModel string) (*Runtime, error) {
	if rt, err := r.resolveRequestedShortcuts(requestedProvider, explicitAPIKey, explicitBaseURL, explicitToken, targetModel); err != nil || rt != nil {
		return rt, err
	}
	if rt, err := r.namedCustomRung(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel); err != nil || rt != nil {
		return rt, err
	}
	if explicitBaseURL == "" && explicitAPIKey == "" {
		if rt := r.localEndpointBypass(requestedProvider, explicitAPIKey, explicitBaseURL); rt != nil {
			return rt, nil
		}
	}
	provider, err := r.resolveProvider(requestedProvider, explicitAPIKey, explicitBaseURL)
	if err != nil {
		return nil, err
	}
	if rt, err := r.resolveExplicitRuntime(provider, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel); err != nil || rt != nil {
		return rt, err
	}
	if rt := r.resolveFromPool(provider, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel); rt != nil {
		return rt, nil
	}
	if err := r.selectedLoginUnavailable(provider, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel); err != nil {
		return nil, err
	}
	var swallowed *AuthError
	if spec, ok := oauthRuntimeProviders[provider]; ok {
		rt, err := r.resolveOAuthRuntime(provider, requestedProvider, targetModel)
		if err != nil {
			authErr, isAuth := err.(*AuthError)
			if !isAuth || requestedProvider != "auto" {
				return nil, err
			}
			pkgLog().Info(spec.failureMsg + "; falling through to next provider.")
			swallowed = authErr
		} else if rt != nil {
			return rt, nil
		}
	}
	if provider == "minimax-oauth" {
		if rt, err := r.minimaxOAuthRuntime(provider, requestedProvider); err != nil || rt != nil {
			return rt, err
		}
	}
	if isExternalProcessProvider(r.ctx, provider) {
		return r.resolveExternalProcessRuntime(provider, requestedProvider)
	}
	if provider == "anthropic" {
		return r.anthropicEnvRuntime(requestedProvider, targetModel)
	}
	if provider == "bedrock" {
		return r.resolveBedrockRuntime(requestedProvider, targetModel)
	}
	if pconfig, ok := registryProfileCtx(r.ctx, provider); ok && pconfig.AuthType == AuthTypeAPIKey {
		return r.apiKeyProviderRuntime(provider, pconfig, requestedProvider, targetModel)
	}
	fallback := r.openrouterFallback(requestedProvider, explicitAPIKey, explicitBaseURL)
	if swallowed != nil && fallback.APIKey == "" && fallback.TokenSource == nil {
		fallback.AuthError = swallowed
	}
	return fallback, nil
}

// Walks the ladder and returns the full runtime for a requested provider
func (r *resolver) ResolveRuntime(requested, explicitAPIKey, explicitBaseURL string, explicitToken func(context.Context) (string, error), targetModel string) (*Runtime, error) {
	requestedProvider := r.resolveRequestedProvider(requested)
	if err := r.raiseIfProviderDisabled(requestedProvider); err != nil {
		return nil, err
	}
	requestedAlias := requestedProvider
	requestedProvider, explicitBaseURL = r.expandDirectAPIAlias(requestedProvider, explicitBaseURL)
	if err := r.raiseIfLocalAliasMissingEndpoint(requestedProvider, explicitBaseURL); err != nil {
		return nil, err
	}
	rt, err := r.ladderRungs(requestedProvider, explicitAPIKey, explicitBaseURL, explicitToken, targetModel)
	if err != nil {
		return nil, err
	}
	if err := raiseForCredentiallessBareCustom(requestedProvider, rt); err != nil {
		return nil, err
	}
	mode := r.maybeApplyCodexAppServerRuntime(rt.Provider, rt.APIMode, requestedAlias)
	if mode != rt.APIMode {
		pkgLog().Info(fmt.Sprintf("model.openai_runtime=codex_app_server overrides the %s runtime (source=%s); its credential/endpoint "+
			"is not used — the app-server authenticates with its own login", rt.Provider, rt.Source))
	}
	rt.APIMode = mode
	if err := r.raiseIfDeclaredKeyMissing(requestedProvider, rt, explicitAPIKey); err != nil {
		return nil, err
	}
	return rt, nil
}

// Resolves the runtime of a requested provider for the home with explicit endpoint overrides
func ResolveRuntimeProvider(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*Runtime, error) {
	return modelsResolver(ctxForHome(ctx, h)).ResolveRuntime(requested, explicitAPIKey, explicitBaseURL, nil, targetModel)
}

// FormatRuntimeProviderError renders a resolution failure as concise user facing guidance
func FormatRuntimeProviderError(err error) string {
	if authErr, ok := err.(*AuthError); ok {
		return formatAuthError(authErr)
	}
	return err.Error()
}

// Log phrase and user phrase for a primary provider failure that triggers the fallback chain
func primaryFailureWording(err *AuthError) (string, string) {
	if isRateLimitedAuthError(err) {
		return "rate-limited (429)", "Primary provider quota exhausted"
	}
	return "auth failed", "Primary auth failed"
}

// PrimaryFailureWording returns the log phrase and user phrase of a primary provider failure that triggers the fallback chain
func PrimaryFailureWording(err *AuthError) (string, string) { return primaryFailureWording(err) }

// ResolveRuntimeWithFallback resolves the primary and walks the fallback chain on a credential failure
func (r *resolver) ResolveRuntimeWithFallback(requested, targetModel, explicitBaseURL, explicitAPIKey string) (*Runtime, map[string]any, error) {
	rt, primaryErr := r.ResolveRuntime(requested, explicitAPIKey, explicitBaseURL, nil, targetModel)
	if primaryErr == nil {
		return rt, nil, nil
	}
	primary, isAuth := primaryErr.(*AuthError)
	if !isAuth {
		return nil, nil, primaryErr
	}
	for _, entry := range FallbackChain(r.hookCtx()) {
		provider := strings.ToLower(strings.TrimSpace(cfgString(entry["provider"])))
		model := strings.TrimSpace(cfgString(entry["model"]))
		if provider == "" || model == "" {
			continue
		}
		baseURL := cfgString(entry["base_url"])
		entryKey := r.ResolveEntryAPIKey(entry)
		fb := newResolver(r.ctx, r.secrets, r.home, r.creds)
		runtime, err := fb.ResolveRuntime(provider, entryKey, baseURL, nil, model)
		if err != nil {
			if _, ok := err.(*AuthError); ok {
				pkgLog().Debug(fmt.Sprintf("Fallback entry %s/%s failed: %v", provider, model, err))
			} else {
				pkgLog().Warn(fmt.Sprintf("Fallback entry %s/%s is misconfigured and was skipped: %v", provider, model, err))
			}
			continue
		}
		runtime.Provider = EffectiveRuntimeProvider(entry, runtime)
		phrase, _ := primaryFailureWording(primary)
		pkgLog().Warn(fmt.Sprintf("Primary provider %s (%v). Falling back to %s/%s", phrase, primary, provider, model))
		return runtime, entry, nil
	}
	return nil, nil, primary
}

var localServerAliases = map[string]bool{"ollama": true, "vllm": true, "llamacpp": true, "llama.cpp": true, "llama-cpp": true}

// True when a first class provider keeps its identity alongside an explicit base URL
func (r *resolver) preserveProviderWithBaseURL(provider string) bool {
	return auxPreserveProviderWithBaseURL(r.ctx, provider)
}

// Resolves an explicit provider moa to its preset's aggregator slot
func (r *resolver) unwrapMoaProvider(prov, mdl string) (string, string) {
	if strings.ToLower(strings.TrimSpace(prov)) != "moa" {
		return prov, mdl
	}
	if aggProvider, aggModel, ok := resolveMoaAggregator(r.hookCtx(), mdl); ok && aggProvider != "" && aggModel != "" {
		return aggProvider, aggModel
	}
	return prov, mdl
}

// auxRoute is the provider, model, base URL, api key, and api mode an auxiliary task resolved to
type auxRoute struct {
	provider, model, baseURL, apiKey string
	apiMode                          APIMode
}

// Provider, model, base URL, key, and api mode for a task, explicit args beating config beating auto
func (r *resolver) resolveTaskProviderModel(task any, provider, model, baseURL, apiKey string) auxRoute {
	var cfgProvider, cfgModel, cfgBaseURL, cfgAPIKey string
	var resolvedMode APIMode
	if auxTaskName(task) != "" {
		tc := r.auxiliaryTaskConfig(task, map[string]bool{})
		cfgProvider = strings.TrimSpace(cfgString(tc["provider"]))
		cfgModel = strings.TrimSpace(cfgString(tc["model"]))
		cfgBaseURL = strings.TrimSpace(cfgString(tc["base_url"]))
		cfgAPIKey = strings.TrimSpace(cfgString(tc["api_key"]))
		if cfgAPIKey == "" {
			if keyEnv := strings.TrimSpace(cfgString(tc["key_env"])); keyEnv != "" {
				cfgAPIKey = r.secret(keyEnv)
			}
		}
		resolvedMode = APIMode(strings.ToLower(strings.TrimSpace(cfgString(tc["api_mode"]))))
	}
	if strings.ToLower(model) == "auto" {
		model = ""
	}
	if strings.ToLower(cfgModel) == "auto" {
		cfgModel = ""
	}
	resolvedModel := model
	if resolvedModel == "" {
		resolvedModel = cfgModel
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "moa" {
		provider, resolvedModel = r.unwrapMoaProvider(provider, resolvedModel)
		if strings.ToLower(provider) != "moa" {
			baseURL, apiKey = "", ""
		}
	} else if strings.ToLower(strings.TrimSpace(cfgProvider)) == "moa" {
		cfgProvider, cfgModel = r.unwrapMoaProvider(cfgProvider, resolvedModel)
		if strings.ToLower(cfgProvider) != "moa" {
			resolvedModel = cfgModel
			cfgBaseURL, cfgAPIKey = "", ""
		}
	}
	if provider != "" {
		provider, baseURL = r.expandDirectAPIAlias(provider, baseURL)
	}
	if cfgProvider != "" {
		cfgProvider, cfgBaseURL = r.expandDirectAPIAlias(cfgProvider, cfgBaseURL)
	}
	if provider != "" && provider != "auto" && baseURL == "" && cfgBaseURL != "" && (cfgProvider == "" || cfgProvider == "auto" || cfgProvider == provider) {
		baseURL = cfgBaseURL
		if apiKey == "" {
			apiKey = cfgAPIKey
		}
	}
	if baseURL != "" {
		kept := "custom"
		if r.preserveProviderWithBaseURL(provider) {
			kept = provider
		}
		return auxRoute{kept, resolvedModel, baseURL, apiKey, resolvedMode}
	}
	if provider != "" {
		return auxRoute{provider, resolvedModel, baseURL, apiKey, resolvedMode}
	}
	if cfgBaseURL != "" && cfgAPIKey != "" {
		kept := "custom"
		if localServerAliases[strings.ToLower(strings.TrimSpace(cfgProvider))] {
			kept = cfgProvider
		}
		return auxRoute{kept, resolvedModel, cfgBaseURL, cfgAPIKey, resolvedMode}
	}
	if cfgBaseURL != "" && cfgProvider != "" && cfgProvider != "auto" {
		return auxRoute{cfgProvider, resolvedModel, cfgBaseURL, "", resolvedMode}
	}
	if cfgProvider != "" && cfgProvider != "auto" {
		return auxRoute{cfgProvider, resolvedModel, cfgBaseURL, cfgAPIKey, resolvedMode}
	}
	return auxRoute{"auto", resolvedModel, "", "", resolvedMode}
}
