package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
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

// ActiveLoginSource names the auth store active provider when it reports logged in
type ActiveLoginSource interface {
	ActiveLoggedInProvider(skipFreeTier bool) string
}

// FreeTierSource reports whether the Nous welcome trial carries inference for this install
type FreeTierSource interface {
	FreeInferenceActive() bool
}

// CredentialErrorSource explains why a provider has no usable login
type CredentialErrorSource interface {
	CredentialError(provider string) *AuthError
}

// Runtime is one resolved provider runtime, the dict hermes hands the agent
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

// resolver carries the config, secrets, home, and logins one resolution walks with
type resolver struct {
	ctx      context.Context
	cfg      map[string]any
	secrets  *config.Secrets
	home     string
	creds    CredentialSource
	cfgError error
	modelCfg map[string]any
	http     *http.Client
}

func newResolver(ctx context.Context, cfg config.Tree, secrets *config.Secrets, h string, creds CredentialSource) *resolver {
	if ctx == nil {
		ctx = context.Background()
	}
	m := map[string]any(cfg)
	if m == nil {
		m = map[string]any{}
	}
	return &resolver{ctx: ctx, cfg: m, secrets: secrets, home: h, creds: creds, http: &http.Client{}}
}

// Process global env names a routed profile may still read from the environment
var globalEnvExact = map[string]bool{
	"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_GATEWAY_LOCK_DIR": true, "ICEBATH_MAX_ITERATIONS": true,
	"ICEBATH_API_TIMEOUT": true, "ICEBATH_REDACT_SECRETS": true, "ICEBATH_NOUS_TIMEOUT_SECONDS": true, "_ICEBATH_GATEWAY": true,
	"PATH": true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true, "TZ": true, "PWD": true, "SHELL": true, "TMPDIR": true,
	"VIRTUAL_ENV": true, "PYTHONPATH": true, "SSL_CERT_FILE": true,
	"ICEBATH_KANBAN_DB": true, "ICEBATH_KANBAN_WORKSPACES_ROOT": true, "ICEBATH_KANBAN_BOARD": true,
	"API_SERVER_ENABLED": true, "API_SERVER_HOST": true, "API_SERVER_PORT": true, "API_SERVER_CORS_ORIGINS": true,
	"GATEWAY_RELAY_URL": true, "GATEWAY_RELAY_ENDPOINT": true, "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS": true,
	"GATEWAY_RELAY_PLATFORMS": true, "GATEWAY_RELAY_BOT_IDS": true, "GATEWAY_RELAY_ROUTE_KEYS": true,
	"GATEWAY_RELAY_INSTANCE_ID": true, "GATEWAY_RELAY_WAKE_URL": true, "GATEWAY_RELAY_DISPLAY_NAME": true,
}

var globalEnvPrefixes = []string{"ICEBATH_KANBAN_", "ICEBATH_TELEGRAM_", "TERMINAL_"}

// True for genuinely process global env vars rather than profile secrets
func isGlobalEnv(name string) bool {
	if globalEnvExact[name] {
		return true
	}
	for _, p := range globalEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Process environment value under the get_secret rules, a scoped routed profile never reading another profile's value
func (r *resolver) processEnv(name string) string {
	if boundScope(r.ctx) != nil && !isGlobalEnv(name) && r.servesRoutedProfile() {
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

// Trimmed env URL with the trailing slash dropped, empty when unset
func optionalBaseURLValue(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// ---- model config ----

var localModelHosts = map[string]bool{"localhost": true, "127.0.0.1": true}

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
		slog.Default().Debug(fmt.Sprintf("Auto-detect model from %s failed: %v", baseURL, err))
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

// The model section with model as an alias of default, dict defaults split, and a local single model detected
func (r *resolver) modelConfig() map[string]any {
	if r.modelCfg != nil {
		return r.modelCfg
	}
	cfg := map[string]any{}
	switch m := r.cfg["model"].(type) {
	case string:
		if strings.TrimSpace(m) != "" {
			cfg["default"] = strings.TrimSpace(m)
		}
	case map[string]any:
		cfg = copyAnyMap(m)
		if !truthy(cfg["default"]) && truthy(cfg["model"]) {
			cfg["default"] = cfg["model"]
		}
		if d, ok := cfg["default"].(map[string]any); ok {
			cfgModel, cfgProvider := config.SplitModelDefault(d)
			if cfgProvider == "" {
				cfgProvider = cfgString(m["provider"])
			}
			cfg["default"] = cfgModel
			if cfgProvider != "" && !truthy(cfg["provider"]) {
				cfg["provider"] = cfgProvider
			}
		}
		baseURL := strings.TrimSpace(cfgString(cfg["base_url"]))
		if strings.TrimSpace(cfgString(cfg["default"])) == "" && baseURL != "" && localModelHosts[Hostname(baseURL)] {
			if detected := r.autoDetectLocalModel(baseURL); detected != "" {
				cfg["default"] = detected
			}
		}
	}
	r.modelCfg = cfg
	return cfg
}

// Provider request from the explicit arg, then config, then the bound client or secret ICEBATH_INFERENCE_PROVIDER, else auto
func (r *resolver) resolveRequestedProvider(requested string) string {
	if strings.TrimSpace(requested) != "" {
		return strings.ToLower(strings.TrimSpace(requested))
	}
	if cfgProvider, ok := r.modelConfig()["provider"].(string); ok && strings.TrimSpace(cfgProvider) != "" {
		return strings.ToLower(strings.TrimSpace(cfgProvider))
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
	r, _, err := strictHomeResolver(ctx, h)
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

// Validated api mode from config, alias spellings canonicalized and registered dialects accepted
func (r *resolver) parseAPIMode(raw any) APIMode {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	normalized := strings.ToLower(CanonicalAPIMode(s))
	if normalized == "" {
		return ""
	}
	if validAPIModes[normalized] || RegisteredAPIModes()[normalized] {
		return APIMode(normalized)
	}
	return ""
}

// ParseConfigAPIMode is the validated api_mode of a config value, empty when invalid
func ParseConfigAPIMode(ctx context.Context, raw any) string {
	return string(modelsResolver(ctx).parseAPIMode(raw))
}

// Api mode when no explicit mode applies, URL detection, then the declared transport, then chat
func (r *resolver) fallbackAPIMode(provider, baseURL, model string) APIMode {
	if isActualRouteCtx(r.ctx, provider, baseURL) {
		return ModeChatCompletions
	}
	if detected := detectAPIModeForURL(baseURL); detected != "" {
		return detected
	}
	declared := determineModeCtx(r.ctx, provider, baseURL, model, r.nousAnthropicWire())
	if declared == ModeAnthropicMessages && !onDeclaredAnthropicEndpoint(provider, baseURL) {
		return ModeChatCompletions
	}
	return declared
}

// The configured Nous anthropic wire preference
func (r *resolver) nousAnthropicWire() string {
	if nous, ok := r.cfg["nous"].(map[string]any); ok {
		return cfgString(nous["anthropic_wire"])
	}
	return ""
}

// NousAPIMode is the Portal wire of a model under the configured nous.anthropic_wire
func NousAPIMode(ctx context.Context, model string) string {
	return string(NousMode(model, modelsResolver(ctx).nousAnthropicWire()))
}

// Whether the base URL is the provider's own anthropic protocol endpoint
func onDeclaredAnthropicEndpoint(provider, baseURL string) bool {
	defaultURL := ""
	if pdef, ok := lookupProviderDef(NormalizeProvider(provider)); ok {
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
func (r *resolver) resolvePlainCustomAPIMode(modelCfg map[string]any, baseURL string) APIMode {
	if isActualRouteCtx(r.ctx, "", baseURL) {
		return ModeChatCompletions
	}
	configured := r.parseAPIMode(modelCfg["api_mode"])
	detected := detectAPIModeForURL(baseURL)
	if configured == ModeCodexResponses && detected != ModeCodexResponses {
		target := baseURL
		if target == "" {
			target = "(unknown)"
		}
		slog.Default().Info("Ignoring persisted custom api_mode=codex_responses for non-OpenAI endpoint " + target)
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

func cfgProviderOf(modelCfg map[string]any) string {
	return strings.ToLower(strings.TrimSpace(cfgString(modelCfg["provider"])))
}

// Persisted model.api_mode when valid and recorded for this provider
func (r *resolver) configuredAPIMode(provider string, modelCfg map[string]any) APIMode {
	configured := r.parseAPIMode(modelCfg["api_mode"])
	if configured != "" && providerSupportsExplicitAPIMode(r.ctx, provider, cfgProviderOf(modelCfg)) {
		return configured
	}
	return ""
}

// The caller's target model beats the persisted default
func effectiveModel(modelCfg map[string]any, targetModel string) string {
	if targetModel != "" {
		return targetModel
	}
	return cfgString(modelCfg["default"])
}

// PerModelAPIModeHook is a provider profile hook answering the wire for one model, empty for the declared mode
type PerModelAPIModeHook func(model, apiKey string) APIMode

var (
	perModelHooksMu sync.RWMutex
	perModelHooks   = map[string]PerModelAPIModeHook{}
)

// RegisterPerModelAPIMode installs a profile owned per model wire hook for one canonical provider
func RegisterPerModelAPIMode(provider string, fn PerModelAPIModeHook) {
	perModelHooksMu.Lock()
	defer perModelHooksMu.Unlock()
	if fn == nil {
		delete(perModelHooks, provider)
		return
	}
	perModelHooks[provider] = fn
}

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
	perModelHooksMu.RLock()
	hook := perModelHooks[provider]
	if p := getProviderProfile(ctx, provider); p != nil && p.APIModeForModelHook != nil {
		hook = p.APIModeForModelHook
	}
	perModelHooksMu.RUnlock()
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
func modelAPIMode(ctx context.Context, profile *perModelHookProfile, model, apiKey string) (mode APIMode) {
	defer func() {
		if rec := recover(); rec != nil {
			apiModeWarnedMu.Lock()
			first := !apiModeWarned[profile.name]
			apiModeWarned[profile.name] = true
			apiModeWarnedMu.Unlock()
			if first {
				slog.Default().WarnContext(ctx, fmt.Sprintf("provider %s: api_mode_for_model failed; using its declared api_mode", profile.name),
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
	if hook == nil {
		perModelHooksMu.RLock()
		hook = perModelHooks[profile.Name]
		perModelHooksMu.RUnlock()
	}
	if hook == nil && profile.PerModelMode != nil {
		hook = func(m, _ string) APIMode { return profile.PerModelMode(m) }
	}
	if hook == nil {
		return "", false
	}
	declared := profile.Mode
	if declared == "" {
		declared = ModeChatCompletions
	}
	mode := modelAPIMode(ctx, &perModelHookProfile{name: profile.Name, mode: declared, hook: hook}, model, "")
	return string(mode), mode != ""
}

// Api mode for a provider that routes per model, its hard per model wire beating a stale persisted one
func (r *resolver) profileRuntimeAPIMode(provider string, modelCfg map[string]any, apiKey, targetModel string) APIMode {
	profile := perModelProfileCtx(r.ctx, provider)
	configured := r.configuredAPIMode(provider, modelCfg)
	modelName := strings.TrimSpace(effectiveModel(modelCfg, targetModel))
	if configured != "" {
		if configured == ModeCodexResponses && modelName != "" && profile != nil && modelAPIMode(r.ctx, profile, modelName, "") == ModeChatCompletions {
			return ModeChatCompletions
		}
		return configured
	}
	if profile == nil {
		return ModeChatCompletions
	}
	if modelName != "" {
		if m := modelAPIMode(r.ctx, profile, modelName, apiKey); m != "" {
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
func (r *resolver) configuredOrFallbackAPIMode(provider string, modelCfg map[string]any, baseURL, model string, opencodeByModel bool) APIMode {
	if provider == "actual" {
		if configured := r.configuredAPIMode(provider, modelCfg); configured != "" && configured != ModeChatCompletions {
			slog.Default().Info(fmt.Sprintf("Routing built-in Actual through chat_completions instead of persisted api_mode=%s", configured))
		}
		return ModeChatCompletions
	}
	if opencodeByModel && OpenCodeFamily(provider) != "" {
		return OpenCodeModelMode(provider, model)
	}
	if configured := r.configuredAPIMode(provider, modelCfg); configured != "" {
		return configured
	}
	return r.fallbackAPIMode(provider, baseURL, model)
}

// Api mode for a registry api_key provider on the explicit and env paths
func (r *resolver) apiKeyProviderAPIMode(provider string, modelCfg map[string]any, apiKey, baseURL, model string, opencodeByModel bool) APIMode {
	if perModelProfileCtx(r.ctx, provider) != nil {
		return r.profileRuntimeAPIMode(provider, modelCfg, apiKey, model)
	}
	if provider == "xai" {
		return ModeCodexResponses
	}
	return r.configuredOrFallbackAPIMode(provider, modelCfg, baseURL, model, opencodeByModel)
}

// Opt in rewrite to codex_app_server via model.openai_runtime for openai, codex, and named custom providers
func (r *resolver) maybeApplyCodexAppServerRuntime(provider string, apiMode APIMode, modelCfg map[string]any, requestedProvider string) APIMode {
	if modelCfg == nil || strings.ToLower(strings.TrimSpace(cfgString(modelCfg["openai_runtime"]))) != "codex_app_server" {
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

// model.base_url only when model.provider is this provider so a stale URL never leaks across providers
func configBaseURLForProvider(modelCfg map[string]any, provider string) string {
	return configBaseURLForProviderCtx(context.Background(), modelCfg, provider)
}

// Config base url for the provider judged against the registry of the home bound to ctx
func configBaseURLForProviderCtx(ctx context.Context, modelCfg map[string]any, provider string) string {
	configured := cfgProviderOf(modelCfg)
	if provider == "actual" {
		configured = NormalizeProvider(configured)
	}
	if !sameRegisteredProvider(ctx, provider, configured) {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(cfgString(modelCfg["base_url"])), "/")
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

// Config base URL for native Anthropic, empty when absent or untrustworthy
func anthropicCfgBaseURL(modelCfg map[string]any) string {
	cfgBaseURL := configBaseURLForProvider(modelCfg, "anthropic")
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

// User set NOUS_INFERENCE_BASE_URL override with the trailing slash stripped
func (r *resolver) nousInferenceEnvOverride() string {
	return optionalBaseURLValue(r.secret("NOUS_INFERENCE_BASE_URL"))
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

// ---- usable secrets (hermes_cli/auth.py) ----

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

// The value stripped when it is a usable prefix valid secret, empty after warning on a prefix mismatch
func usableDeclaredSecret(providerID, value, source string) string {
	val := strings.TrimSpace(value)
	if !hasUsableSecret(val) {
		return ""
	}
	if prefixes := knownProviderKeyPrefixes[providerID]; len(prefixes) > 0 && !matchesKeyPrefix(providerID, val) {
		slog.Default().Warn(fmt.Sprintf("Ignoring %s for provider %q: value does not match the expected key prefix (%s). "+
			"Falling back to the next credential source. Fix or remove the malformed key to silence this warning.",
			source, providerID, strings.Join(prefixes, " or ")))
		return ""
	}
	return val
}

// ---- api key provider credentials (hermes_cli/auth.py resolve_api_key_provider_credentials) ----

// apiKeyCreds is a resolved api key provider login with its base URL and source
type apiKeyCreds struct {
	APIKey      string
	BaseURL     string
	Source      string
	TokenSource func(ctx context.Context) (string, error)
}

// model.key_env when config.yaml's main model targets the provider
func (r *resolver) modelLevelKeyEnv(providerID string) string {
	modelCfg, ok := r.cfg["model"].(map[string]any)
	if !ok {
		return ""
	}
	if strings.ToLower(strings.TrimSpace(cfgString(modelCfg["provider"]))) != providerID {
		return ""
	}
	return strings.TrimSpace(firstTruthyString(modelCfg["key_env"], modelCfg["api_key_env"]))
}

// APIKeySecretResolver is a provider owned credential resolution returning the runtime key and its source
type APIKeySecretResolver func() (apiKey, source string, ok bool)

// RuntimeBaseURLResolver is a provider owned base URL derived from the credential, empty for env or default
type RuntimeBaseURLResolver func(apiKey, defaultURL, envURL string) string

var (
	credentialSeamsMu   sync.RWMutex
	apiKeySecretSeams   = map[string]APIKeySecretResolver{}
	runtimeBaseURLSeams = map[string]RuntimeBaseURLResolver{}
)

// RegisterAPIKeySecretResolver installs a profile owned api key resolution that is final when it answers
func RegisterAPIKeySecretResolver(provider string, fn APIKeySecretResolver) {
	credentialSeamsMu.Lock()
	defer credentialSeamsMu.Unlock()
	if fn == nil {
		delete(apiKeySecretSeams, provider)
		return
	}
	apiKeySecretSeams[provider] = fn
}

// RegisterRuntimeBaseURLResolver installs a profile owned runtime base URL derived from the credential
func RegisterRuntimeBaseURLResolver(provider string, fn RuntimeBaseURLResolver) {
	credentialSeamsMu.Lock()
	defer credentialSeamsMu.Unlock()
	if fn == nil {
		delete(runtimeBaseURLSeams, provider)
		return
	}
	runtimeBaseURLSeams[provider] = fn
}

// Api key provider secret and its source, the profile seam, model.key_env, registry env vars, then the login store
func (r *resolver) apiKeyProviderSecret(providerID string, pconfig Profile) (string, string, func(context.Context) (string, error)) {
	credentialSeamsMu.RLock()
	owned := apiKeySecretSeams[providerID]
	if p := getProviderProfile(r.ctx, providerID); p != nil && p.ResolveAPIKeySecretHook != nil {
		owned = p.ResolveAPIKeySecretHook
	}
	credentialSeamsMu.RUnlock()
	if owned != nil {
		if key, source, ok := owned(); ok {
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
		if model, ok := r.cfg["model"].(map[string]any); ok && NormalizeProvider(cfgString(model["provider"])) == "actual" {
			if configured := strings.TrimSpace(cfgString(model["base_url"])); configured != "" {
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
	credentialSeamsMu.RLock()
	urlSeam := runtimeBaseURLSeams[providerID]
	if p := getProviderProfile(r.ctx, providerID); p != nil && p.RuntimeBaseURLHook != nil {
		urlSeam = p.RuntimeBaseURLHook
	}
	credentialSeamsMu.RUnlock()
	baseURL := ""
	if urlSeam != nil {
		baseURL = urlSeam(apiKey, pconfig.BaseURL, envURL)
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
	resolved, err := r.resolveProvider(name, "", "", false)
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
	if r.cfgError == nil {
		return nil
	}
	path := r.home + "/config.yaml"
	return newAuthErr("", fmt.Sprintf("config.yaml at %s is corrupt (%v) — refusing to auto-select "+
		"an inference provider from environment keys. Fix the YAML (a backup "+
		"was saved next to it) or run icebath setup.", path, r.cfgError), "corrupt_config", false)
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

// auth.json active_provider when it is a registry provider that reports logged in
func (r *resolver) loggedInOAuthActiveProvider(skipFreeTier bool) string {
	src, ok := r.creds.(ActiveLoginSource)
	if !ok {
		return ""
	}
	active := src.ActiveLoggedInProvider(skipFreeTier)
	if _, known := registryProfileCtx(r.ctx, active); !known {
		return ""
	}
	return active
}

// Model config and the provider config.yaml names when it pins a registry provider or a custom endpoint
func (r *resolver) configModelProvider() (map[string]any, string) {
	modelCfg, _ := r.cfg["model"].(map[string]any)
	provider := ""
	if modelCfg != nil {
		provider = strings.ToLower(strings.TrimSpace(cfgString(modelCfg["provider"])))
	}
	provider = authNormalizeProvider(provider)
	if provider == "custom" || strings.HasPrefix(provider, "custom:") {
		return modelCfg, "custom"
	}
	if _, ok := registryProfileCtx(r.ctx, provider); provider == "openrouter" || ok {
		return modelCfg, provider
	}
	if provider != "" && r.HasNamedCustomProvider(provider) {
		return modelCfg, "custom"
	}
	baseURL := ""
	if modelCfg != nil {
		baseURL = strings.TrimSpace(cfgString(modelCfg["base_url"]))
	}
	if baseURL != "" && r.configBaseURLTrustworthyForBareCustom(baseURL, provider) {
		return modelCfg, "custom"
	}
	return modelCfg, ""
}

var noAutoDetectProviders = map[string]bool{"lmstudio": true}

// First registry api key provider in registry order with a usable env key
func (r *resolver) envKeyAutoDetected(oauthActive string) string {
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
				if oauthActive != "" && oauthActive != pid {
					slog.Default().Warn(fmt.Sprintf("Provider resolved to %q via %s, preempting your logged-in OAuth provider %q. "+
						"If you meant to use the OAuth login, unset %s or set `model.provider` explicitly.", pid, envVar, oauthActive, envVar))
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
	issues := configStructureIssueLines(r.cfg)
	if len(issues) == 0 {
		return ""
	}
	lines := append([]string{"Config issue detected — run 'icebath doctor' for full diagnostics:"}, issues...)
	return strings.Join(lines, "\n")
}

// Which inference provider to use, explicit intent beating a stale OAuth login on the auto path
func (r *resolver) resolveProvider(requested, explicitAPIKey, explicitBaseURL string, skipFreeTier bool) (string, error) {
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
	modelCfg, cfgProvider := r.configModelProvider()
	if cfgProvider != "" {
		return cfgProvider, nil
	}
	if r.openrouterAutoDetected() {
		if err := r.refuseEnvAdoptionIfConfigCorrupt(); err != nil {
			return "", err
		}
		return "openrouter", nil
	}
	oauthActive := r.loggedInOAuthActiveProvider(skipFreeTier)
	if envPid := r.envKeyAutoDetected(oauthActive); envPid != "" {
		return envPid, nil
	}
	if oauthActive != "" {
		if len(modelCfg) > 0 && !truthy(modelCfg["provider"]) {
			slog.Default().Warn(fmt.Sprintf("Provider resolved to logged-in OAuth provider %q because config.yaml `model` has no `provider` key. "+
				"If you meant a different provider, set `model.provider` explicitly.", oauthActive))
		}
		return oauthActive, nil
	}
	if !skipFreeTier {
		if ft, ok := r.creds.(FreeTierSource); ok && ft.FreeInferenceActive() {
			return "nous", nil
		}
	}
	if hasAWSCredentials(true) {
		return "bedrock", nil
	}
	return "", newAuthErr("", fmt.Sprintf("Icebath is not connected to any AI provider yet. Run `icebath model` to pick one, type `/login` in chat, "+
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
func (r *resolver) poolEntryModeAndURL(provider string, cred Credential, modelCfg map[string]any, model, baseURL string) (APIMode, string) {
	if provider == "actual" && strings.HasPrefix(strings.TrimPrefix(cred.Source, "pool:"), "env:") {
		if cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider); cfgURL != "" {
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
				if cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider); cfgURL != "" {
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
				if cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider); cfgURL != "" {
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
		u := anthropicCfgBaseURL(modelCfg)
		if u == "" {
			u = baseURL
		}
		if u == "" {
			u = AnthropicBaseURL
		}
		return ModeAnthropicMessages, u
	}
	if provider == "nous" {
		u := r.nousInferenceEnvOverride()
		if u == "" {
			u = baseURL
		}
		return NousMode(model, r.nousAnthropicWire()), u
	}
	if perModelProfileCtx(r.ctx, provider) != nil {
		mode := r.profileRuntimeAPIMode(provider, modelCfg, cred.APIKey, model)
		if baseURL == "" {
			if p, ok := ProfileFor(provider); ok {
				baseURL = p.BaseURL
			}
		}
		return mode, baseURL
	}
	if provider == "azure-foundry" {
		mode := ModeChatCompletions
		if cfgProviderOf(modelCfg) == "azure-foundry" {
			if cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, "azure-foundry"); cfgURL != "" {
				baseURL = cfgURL
			}
			if parsed := r.parseAPIMode(modelCfg["api_mode"]); parsed != "" {
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
		if cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider); cfgURL != "" {
			baseURL = cfgURL
		} else if baseURL == "" {
			baseURL = pconfig.BaseURL
		}
	}
	return r.configuredOrFallbackAPIMode(provider, modelCfg, baseURL, model, true), baseURL
}

// Runtime from a selected login of the provider
func (r *resolver) runtimeFromPoolEntry(provider string, cred Credential, requestedProvider string, modelCfg map[string]any, targetModel string) *Runtime {
	mode, baseURL := r.poolEntryModeAndURL(provider, cred, modelCfg, effectiveModel(modelCfg, targetModel), strings.TrimRight(cred.BaseURL, "/"))
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
func (r *resolver) openrouterShouldUsePool(requestedProvider string, modelCfg map[string]any, explicitAPIKey, explicitBaseURL string) bool {
	cfgBaseURL := strings.TrimSpace(cfgString(modelCfg["base_url"]))
	envBase := strings.TrimSpace(r.secret("OPENAI_BASE_URL"))
	if envBase == "" {
		envBase = strings.TrimSpace(r.secret("OPENROUTER_BASE_URL"))
	}
	cfgProv := cfgProviderOf(modelCfg)
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
func (r *resolver) resolveFromPool(provider, requestedProvider string, modelCfg map[string]any, explicitAPIKey, explicitBaseURL, targetModel string) *Runtime {
	if provider == "openrouter" && !r.openrouterShouldUsePool(requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL) {
		return nil
	}
	cred, ok := r.credentialForModel(provider, targetModel)
	if !ok {
		return nil
	}
	if cred.TokenSource == nil && !hasUsableSecret(cred.APIKey) {
		return nil
	}
	return r.runtimeFromPoolEntry(provider, cred, requestedProvider, modelCfg, targetModel)
}

// ---- explicit --api-key / --base-url path ----

func (r *resolver) explicitAnthropic(requestedProvider string, modelCfg map[string]any, apiKey, baseURL, targetModel string) (*Runtime, error) {
	if baseURL == "" {
		baseURL = anthropicCfgBaseURL(modelCfg)
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
		if src, ok := r.creds.(CredentialErrorSource); ok {
			if err := src.CredentialError(provider); err != nil {
				return Credential{}, err
			}
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
	case "nous":
		return nousErr("Icebath is not logged into Nous Portal.", "nous_auth_missing", true)
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

func (r *resolver) explicitCodex(requestedProvider string, modelCfg map[string]any, apiKey, explicitBaseURL, targetModel string) (*Runtime, error) {
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

func (r *resolver) explicitQwen(requestedProvider string, modelCfg map[string]any, apiKey, explicitBaseURL, targetModel string) (*Runtime, error) {
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

func (r *resolver) explicitNous(requestedProvider string, modelCfg map[string]any, apiKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	base := explicitBaseURL
	if base == "" {
		base = r.nousInferenceEnvOverride()
	}
	if base == "" {
		base = NousInferenceBaseURL
	}
	key, baseURL, src, source, err := r.credsFallback("nous", apiKey, explicitBaseURL, base)
	if err != nil {
		return nil, err
	}
	rt := r.runtime("nous", NousMode(effectiveModel(modelCfg, targetModel), r.nousAnthropicWire()), baseURL, key, source)
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

func (r *resolver) explicitAPIKeyProvider(provider string, pconfig Profile, requestedProvider string, modelCfg map[string]any, apiKey, baseURL, targetModel string) (*Runtime, error) {
	var tokenSrc func(context.Context) (string, error)
	if baseURL == "" {
		switch {
		case provider == "actual":
			baseURL = configBaseURLForProviderCtx(r.ctx, modelCfg, provider)
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
		model = cfgString(modelCfg["default"])
	}
	mode := r.apiKeyProviderAPIMode(provider, modelCfg, apiKey, baseURL, model, true)
	baseURL = finalizeBaseURL(provider, mode, baseURL)
	apiKey = actualLocalKey(provider, apiKey, baseURL)
	rt := r.runtime(provider, mode, strings.TrimRight(baseURL, "/"), apiKey, "explicit")
	rt.TokenSource = tokenSrc
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Runtime from explicit CLI credentials, nil when none were given or the provider takes none
func (r *resolver) resolveExplicitRuntime(provider, requestedProvider string, modelCfg map[string]any, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	explicitAPIKey = strings.TrimSpace(explicitAPIKey)
	explicitBaseURL = strings.TrimRight(strings.TrimSpace(explicitBaseURL), "/")
	if explicitAPIKey == "" && explicitBaseURL == "" {
		return nil, nil
	}
	switch provider {
	case "anthropic":
		return r.explicitAnthropic(requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel)
	case "openai-codex":
		return r.explicitCodex(requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel)
	case "nous":
		return r.explicitNous(requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel)
	case "qwen-oauth":
		return r.explicitQwen(requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel)
	case "azure-foundry":
		return r.resolveAzureFoundryRuntime(requestedProvider, modelCfg, explicitAPIKey, nil, explicitBaseURL, targetModel)
	}
	pconfig, ok := registryProfileCtx(r.ctx, provider)
	if !ok || pconfig.AuthType != AuthTypeAPIKey {
		return nil, nil
	}
	return r.explicitAPIKeyProvider(provider, pconfig, requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel)
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
	"nous": {func(m string, r *resolver) APIMode { return NousMode(m, r.nousAnthropicWire()) }, "portal",
		"Auto-detected Nous provider but credentials failed", ""},
	"openai-codex": {func(string, *resolver) APIMode { return ModeCodexResponses }, "icebath-auth-store",
		"Auto-detected Codex provider but credentials failed", ""},
	"xai-oauth": {func(string, *resolver) APIMode { return ModeCodexResponses }, "icebath-auth-store",
		"Auto-detected xAI OAuth provider but credentials failed", DefaultXAIOAuthBaseURL},
	"qwen-oauth": {func(string, *resolver) APIMode { return ModeChatCompletions }, "qwen-cli",
		"Qwen OAuth credentials failed", ""},
}

// Runtime from an oauth spec, raising when the stored login is stale, revoked, or benched
func (r *resolver) resolveOAuthRuntime(provider, requestedProvider string, modelCfg map[string]any, targetModel string) (*Runtime, error) {
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
	rt := r.runtime(provider, spec.mode(effectiveModel(modelCfg, targetModel), r), baseURL, cred.APIKey, source)
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

// Azure Anthropic key from key_env hints, an inline api_key, then the historical fixed names
func (r *resolver) azureAnthropicEnvKey(modelCfg map[string]any) string {
	for _, hint := range []string{"key_env", "api_key_env"} {
		if envVar := strings.TrimSpace(cfgString(modelCfg[hint])); envVar != "" {
			if token := strings.TrimSpace(r.secret(envVar)); token != "" {
				return token
			}
		}
	}
	if inline := strings.TrimSpace(cfgString(modelCfg["api_key"])); inline != "" {
		return inline
	}
	return r.firstSecret("AZURE_ANTHROPIC_KEY", "ANTHROPIC_API_KEY")
}

// Native Anthropic from env and the login store, model.base_url honoured only for provider anthropic
func (r *resolver) anthropicEnvRuntime(requestedProvider string, modelCfg map[string]any, targetModel string) (*Runtime, error) {
	baseURL := anthropicCfgBaseURL(modelCfg)
	if baseURL == "" {
		baseURL = AnthropicBaseURL
	}
	var token string
	var src func(context.Context) (string, error)
	if HostMatches(baseURL, "azure.com") {
		token = r.azureAnthropicEnvKey(modelCfg)
		if token == "" {
			return nil, newAuthErr("anthropic", "No Azure Anthropic API key found. Set AZURE_ANTHROPIC_KEY or ANTHROPIC_API_KEY, or point "+
				"key_env/api_key_env in your config.yaml model section at a custom env var.", "", false)
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
func (r *resolver) apiKeyProviderRuntime(provider string, pconfig Profile, requestedProvider string, modelCfg map[string]any, targetModel string) (*Runtime, error) {
	creds, err := r.apiKeyProviderCredentials(provider)
	if err != nil {
		return nil, err
	}
	if provider == "actual" && creds.TokenSource == nil && !hasUsableSecret(creds.APIKey) {
		cfgURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider)
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
	baseURL := configBaseURLForProviderCtx(r.ctx, modelCfg, provider)
	if baseURL == "" {
		baseURL = strings.TrimRight(creds.BaseURL, "/")
	}
	baseURL = actualURL(provider, baseURL)
	model := targetModel
	if model == "" {
		model = cfgString(modelCfg["default"])
	}
	mode := r.apiKeyProviderAPIMode(provider, modelCfg, creds.APIKey, baseURL, model, true)
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
				slog.Default().Warn(name + " missing_credentials_hint failed; showing the error without it")
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
	provs, ok := r.cfg["providers"].(map[string]any)
	if !ok {
		return nil
	}
	block, ok := provs[requestedProvider].(map[string]any)
	if ok && !IsProviderEnabled(block) {
		return fmt.Errorf("provider %q is disabled in config (providers.%s.enabled: false)", requestedProvider, requestedProvider)
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
	modelCfg := r.modelConfig()
	if r.configBaseURLTrustworthyForBareCustom(cfgString(modelCfg["base_url"]), cfgProviderOf(modelCfg)) {
		return nil
	}
	if entry := r.getNamedCustomProvider(requestedProvider, nil, true); entry != nil && strings.TrimSpace(cfgString(entry["base_url"])) != "" {
		return nil
	}
	return newAuthErr(requestedProvider, fmt.Sprintf("provider '%s' has no endpoint configured, so the request is not sent anywhere "+
		"(it would otherwise fall back to OpenRouter). Set providers.%s.base_url or model.base_url in config.yaml.",
		requestedProvider, norm), "missing_base_url", false)
}

// Vertex AI with a short lived token minted per call rather than a static key
func (r *resolver) resolveVertexRuntime(requestedProvider string) (*Runtime, error) {
	tokenSrc, baseURL, ok := vertexRuntimeConfig(r.ctx, r.cfg, r.secrets, r.home)
	if !ok || tokenSrc == nil || baseURL == "" {
		return nil, newAuthErr("vertex", "Vertex AI credentials could not be resolved. Vertex uses OAuth2 (not a static API key): provide a "+
			"service-account JSON via GOOGLE_APPLICATION_CREDENTIALS (or VERTEX_CREDENTIALS_PATH) in ~/.icebath/.env, "+
			"or run 'gcloud auth application-default login' for ADC. Set the GCP project/region under vertex: in "+
			"config.yaml if they aren't embedded in the credentials. Run `icebath setup` to install Vertex support.", "", false)
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
			key = r.azureAnthropicEnvKey(map[string]any{})
		}
		rt := r.runtime("anthropic", ModeAnthropicMessages, strings.TrimRight(effBase, "/"), key, "azure-explicit")
		rt.RequestedProvider = requestedProvider
		return rt, nil
	}
	if requestedProvider == "azure-foundry" {
		return r.resolveAzureFoundryRuntime(requestedProvider, r.modelConfig(), explicitAPIKey, explicitToken, explicitBaseURL, targetModel)
	}
	if vertexNames[requestedProvider] {
		return r.resolveVertexRuntime(requestedProvider)
	}
	return nil, nil
}

// Rung 3, a configured named custom provider honouring the codex app server opt in
func (r *resolver) namedCustomRung(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	rt, err := r.resolveNamedCustomRuntime(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel, nil)
	if err != nil || rt == nil {
		return rt, err
	}
	rt.RequestedProvider = requestedProvider
	if rt.Provider == "custom" {
		mode := rt.APIMode
		if mode == "" {
			mode = ModeChatCompletions
		}
		rt.APIMode = r.maybeApplyCodexAppServerRuntime("custom", mode, r.modelConfig(), requestedProvider)
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
	provider, err := r.resolveProvider(requestedProvider, explicitAPIKey, explicitBaseURL, false)
	if err != nil {
		return nil, err
	}
	modelCfg := r.modelConfig()
	if rt, err := r.resolveExplicitRuntime(provider, requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel); err != nil || rt != nil {
		return rt, err
	}
	if rt := r.resolveFromPool(provider, requestedProvider, modelCfg, explicitAPIKey, explicitBaseURL, targetModel); rt != nil {
		return rt, nil
	}
	var swallowed *AuthError
	if spec, ok := oauthRuntimeProviders[provider]; ok {
		rt, err := r.resolveOAuthRuntime(provider, requestedProvider, modelCfg, targetModel)
		if err != nil {
			authErr, isAuth := err.(*AuthError)
			if !isAuth || requestedProvider != "auto" {
				return nil, err
			}
			slog.Default().Info(spec.failureMsg + "; falling through to next provider.")
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
		return r.anthropicEnvRuntime(requestedProvider, modelCfg, targetModel)
	}
	if provider == "bedrock" {
		return r.resolveBedrockRuntime(requestedProvider, modelCfg, targetModel)
	}
	if pconfig, ok := registryProfileCtx(r.ctx, provider); ok && pconfig.AuthType == AuthTypeAPIKey {
		return r.apiKeyProviderRuntime(provider, pconfig, requestedProvider, modelCfg, targetModel)
	}
	fallback := r.openrouterFallback(requestedProvider, explicitAPIKey, explicitBaseURL)
	if swallowed != nil && fallback.APIKey == "" && fallback.TokenSource == nil {
		fallback.AuthError = swallowed
	}
	return fallback, nil
}

// ResolveRuntime walks the hermes ladder and returns the full runtime for a requested provider
func (r *resolver) ResolveRuntime(requested, explicitAPIKey, explicitBaseURL string, explicitToken func(context.Context) (string, error), targetModel string) (*Runtime, error) {
	requestedProvider := r.resolveRequestedProvider(requested)
	recoverLeftCoreProvider(r.ctx, r.home, requestedProvider)
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
	mode := r.maybeApplyCodexAppServerRuntime(rt.Provider, rt.APIMode, r.modelConfig(), requestedAlias)
	if mode != rt.APIMode {
		slog.Default().Info(fmt.Sprintf("model.openai_runtime=codex_app_server overrides the %s runtime (source=%s); its credential/endpoint "+
			"is not used — the app-server authenticates with its own login", rt.Provider, rt.Source))
	}
	rt.APIMode = mode
	if err := r.raiseIfDeclaredKeyMissing(requestedProvider, rt, r.modelConfig(), explicitAPIKey); err != nil {
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
	for _, entry := range GetFallbackChain(r.cfg) {
		provider := strings.ToLower(strings.TrimSpace(cfgString(entry["provider"])))
		model := strings.TrimSpace(cfgString(entry["model"]))
		if provider == "" || model == "" {
			continue
		}
		baseURL := cfgString(entry["base_url"])
		entryKey := r.ResolveEntryAPIKey(entry)
		fb := newResolver(r.ctx, r.cfg, r.secrets, r.home, r.creds)
		fb.cfgError = r.cfgError
		runtime, err := fb.ResolveRuntime(provider, entryKey, baseURL, nil, model)
		if err != nil {
			if _, ok := err.(*AuthError); ok {
				slog.Default().Debug(fmt.Sprintf("Fallback entry %s/%s failed: %v", provider, model, err))
			} else {
				slog.Default().Warn(fmt.Sprintf("Fallback entry %s/%s is misconfigured and was skipped: %v", provider, model, err))
			}
			continue
		}
		runtime.Provider = EffectiveRuntimeProvider(entry, runtime)
		phrase, _ := primaryFailureWording(primary)
		slog.Default().Warn(fmt.Sprintf("Primary provider %s (%v). Falling back to %s/%s", phrase, primary, provider, model))
		return runtime, entry, nil
	}
	return nil, nil, primary
}

// ---- the three entry points resolve.go delegates to ----

func resolveRuntimeLadder(ctx context.Context, cfg config.Tree, secrets *config.Secrets, h string, creds CredentialSource, over Overrides) (Route, error) {
	r := newResolver(ctx, cfg, secrets, h, creds)
	r.cfgError = over.ConfigError
	rt, err := r.ResolveRuntime(over.Provider, over.APIKey, over.BaseURL, over.TokenSource, over.Model)
	if err != nil {
		return Route{}, err
	}
	if over.Mode != "" {
		rt.APIMode = over.Mode
	}
	if rt.Model == "" {
		rt.Model = effectiveModel(r.modelConfig(), over.Model)
	}
	return rt.Route(), nil
}

var localServerAliases = map[string]bool{"ollama": true, "vllm": true, "llamacpp": true, "llama.cpp": true, "llama-cpp": true}

// True when a first class provider keeps its identity alongside an explicit base URL
func (r *resolver) preserveProviderWithBaseURL(provider string) bool {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" || normalized == "auto" || normalized == "custom" || strings.HasPrefix(normalized, "custom:") {
		return false
	}
	if localServerAliases[normalized] {
		return true
	}
	if _, ok := registryProfileCtx(r.ctx, authNormalizeProvider(normalized)); ok {
		return true
	}
	return r.HasNamedCustomProvider(normalized)
}

// Resolves an explicit provider moa to its preset's aggregator slot
func (r *resolver) unwrapMoaProvider(prov, mdl string) (string, string) {
	if strings.ToLower(strings.TrimSpace(prov)) != "moa" {
		return prov, mdl
	}
	if aggProvider, aggModel, ok := resolveMoaAggregator(r.cfg, mdl); ok && aggProvider != "" && aggModel != "" {
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
			if keyEnv := strings.TrimSpace(firstTruthyString(tc["key_env"], tc["api_key_env"])); keyEnv != "" {
				cfgAPIKey = r.secret(keyEnv)
			}
		}
		resolvedMode = APIMode(strings.ToLower(CanonicalAPIMode(cfgString(tc["api_mode"]))))
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
	if provider != "" && provider != "auto" && baseURL == "" && cfgBaseURL != "" && (cfgProvider == "" || cfgProvider == provider) {
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

func resolveAuxiliaryRoute(ctx context.Context, cfg config.Tree, secrets *config.Secrets, h string, creds CredentialSource, task string) (Route, error) {
	r := newResolver(ctx, cfg, secrets, h, creds)
	ar := r.resolveTaskProviderModel(task, "", "", "", "")
	requested := ar.provider
	if requested == "auto" {
		requested = ""
	}
	rt, err := r.ResolveRuntime(requested, ar.apiKey, ar.baseURL, nil, ar.model)
	if err != nil {
		return Route{}, err
	}
	if ar.apiMode != "" {
		rt.APIMode = ar.apiMode
	}
	if ar.model != "" {
		rt.Model = ar.model
	} else if rt.Model == "" {
		rt.Model = effectiveModel(r.modelConfig(), "")
	}
	if ar.provider != "auto" && strings.ToLower(ar.provider) != "custom" && rt.Provider == "custom" {
		rt.Provider = EffectiveRuntimeProvider(map[string]any{"provider": ar.provider}, rt)
	}
	return rt.Route(), nil
}

func resolveFallbackChain(ctx context.Context, cfg config.Tree, secrets *config.Secrets, h string, creds CredentialSource) ([]Route, error) {
	r := newResolver(ctx, cfg, secrets, h, creds)
	var routes []Route
	var errs []string
	for _, entry := range GetFallbackChain(r.cfg) {
		provider := strings.ToLower(strings.TrimSpace(cfgString(entry["provider"])))
		model := strings.TrimSpace(cfgString(entry["model"]))
		if provider == "" || model == "" {
			continue
		}
		fb := newResolver(ctx, cfg, secrets, h, creds)
		rt, err := fb.ResolveRuntime(provider, r.ResolveEntryAPIKey(entry), cfgString(entry["base_url"]), nil, model)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", provider, model, err))
			continue
		}
		rt.Provider = EffectiveRuntimeProvider(entry, rt)
		rt.Model = model
		routes = append(routes, rt.Route())
	}
	if len(routes) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("provider: no fallback entry resolved: %s", strings.Join(errs, "; "))
	}
	return routes, nil
}

// Integer env value with a default when unset or malformed
func (r *resolver) envInt(name string, def int) int {
	if v := strings.TrimSpace(r.secret(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
