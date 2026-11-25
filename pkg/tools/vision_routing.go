package tools

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Env.Values key holding the live main runtime a turn bound for auxiliary routing
const runtimeMainKey = "runtime_main"

// VisionCapabilitySource answers model capability questions owned by pkg/provider, installed by the provider wiring
type VisionCapabilitySource interface {
	// Vision flags of the provider profile registered under a name or alias, found false when none is
	ProviderVisionProfile(provider string) (supportsVision, supportsVisionToolMessages, found bool)
	// Reports whether the provider routes vendor prefixed model ids to other providers
	IsRoutingAggregator(provider string) bool
	// Catalog vision verdict with profile model capabilities and overrides applied, known false when nothing knows
	CatalogSupportsVision(ctx context.Context, provider, model string) (supported, known bool)
	// Base slug of a Codex context variant picker id
	StripCodexContextVariantSuffix(model string) string
	// Reports whether a base URL is the managed local server this install launched
	IsManagedEndpoint(baseURL string) bool
	// Ground truth vision support of a staged managed model, known false when the model is not staged
	ManagedModelSupportsVision(ctx context.Context, model string) (supported, known bool)
	// Reports whether a base URL points at a loopback or private network endpoint
	IsLocalEndpoint(baseURL string) bool
	// Server family behind a local endpoint such as ollama, empty when undetected
	DetectLocalServerType(ctx context.Context, baseURL, apiKey string) string
	// Ollama api show verdict for a model, known false when the server gave no answer
	QueryOllamaSupportsVision(ctx context.Context, model, baseURL, apiKey string) (supported, known bool)
}

// VisionCapabilities is the installed capability source, nil leaving every probe without a verdict
var VisionCapabilities VisionCapabilitySource

// Copy of the env with its own slot for the live main runtime of the agent that owns it
func WithRuntimeMainSlot(env *Env) *Env {
	if env == nil {
		return nil
	}
	out := *env
	out.runtimeMain = &atomic.Pointer[map[string]any]{}
	return &out
}

// Binds the live main runtime of a turn on the agent's env, a nil map clearing it
func BindRuntimeMain(env *Env, runtime map[string]any) {
	if env == nil || env.runtimeMain == nil {
		return
	}
	if runtime == nil {
		env.runtimeMain.Store(nil)
		return
	}
	env.runtimeMain.Store(&runtime)
}

// Live main runtime map bound for this session, nil when the turn bound none
func runtimeMain(env *Env) map[string]any {
	if env != nil && env.runtimeMain != nil {
		if bound := env.runtimeMain.Load(); bound != nil {
			return *bound
		}
	}
	if env == nil || env.Values == nil {
		return nil
	}
	v, ok := env.Values.Load(runtimeMainKey)
	if !ok {
		return nil
	}
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out
	}
	return nil
}

// Raw runtime field, nil when unbound or falsy
func runtimeMainValue(env *Env, field string) any {
	if v := runtimeMain(env)[field]; truthy(v) {
		return v
	}
	return nil
}

// Stripped text of a runtime field, empty when unbound
func runtimeMainText(env *Env, field string) string {
	return cleanStr(runtimeMainValue(env, field))
}

// Python str of a value stripped, falsy values reading as empty
func cleanStr(v any) string {
	if !truthy(v) {
		return ""
	}
	return strings.TrimSpace(anyutil.String(v))
}

// Main model field from the live runtime first, then the profile's model settings, lowercased when asked
func readMainField(env *Env, field string, lower bool) string {
	s, _ := runtimeMainValue(env, field).(string)
	if s = strings.TrimSpace(s); s == "" {
		ctx := envHomeCtx(context.Background(), env)
		switch field {
		case "model":
			s = strings.TrimSpace(settings.Model.Get(ctx))
		case "provider":
			s = strings.TrimSpace(settings.ModelProvider.Get(ctx))
		}
	}
	if lower {
		return strings.ToLower(s)
	}
	return s
}

// Main model of the session
func readMainModel(env *Env) string { return readMainField(env, "model", false) }

// Main provider of the session, lowercased
func readMainProvider(env *Env) string { return readMainField(env, "provider", true) }

// Probe credential from a string or a token source, never the source itself
func materializeProbeAPIKey(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case func() string:
		return strings.TrimSpace(t())
	case func() (string, error):
		s, err := t()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	return ""
}

// Context whose home override is the profile home the env is bound to, ctx itself for an env naming none
func envHomeCtx(ctx context.Context, env *Env) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return ctx
}

// Context of a vision check, the env home plus that profile's secret scope while multiplexing with none bound yet
func visionCtx(ctx context.Context, env *Env) context.Context {
	ctx = envHomeCtx(ctx, env)
	if BindProfileSecretScope != nil && contextSecretScope(ctx) == nil && MultiplexActiveFor(ctx, env) {
		ctx = BindProfileSecretScope(ctx, home.Dir(ctx))
	}
	return ctx
}

// Context whose home override is the profile home the env is bound to, ctx itself for an env naming none
func WithEnvHome(ctx context.Context, env *Env) context.Context { return envHomeCtx(ctx, env) }

// Capability boolean from real bools, 0 or 1, and YAML boolean tokens only
func coerceCapabilityBool(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case int:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	case int64:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "on", "1":
			return true, true
		case "false", "no", "off", "0":
			return false, true
		}
	}
	return false, false
}

// Mapping value or nil
func dictOrNil(v any) map[string]any { return config.AsMap(v) }

// Per model supports_vision from a models mapping
func modelSupportsVisionOverride(modelsCfg any, model string) (bool, bool) {
	return coerceCapabilityBool(dictOrNil(dictOrNil(modelsCfg)[model])["supports_vision"])
}

// User declared vision capability from the settings of ctx, the first hit across model and providers
func supportsVisionOverride(ctx context.Context, provider, model, requestedProvider string) (bool, bool) {
	if v, ok := settings.ModelSupportsVision.Lookup(ctx); ok {
		return v, true
	}
	var candidates []string
	seen := map[string]bool{}
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			candidates = append(candidates, c)
		}
	}
	for _, c := range []string{requestedProvider, provider, strings.TrimSpace(settings.ModelProvider.Get(ctx))} {
		if c == "" {
			continue
		}
		add(c)
		if strings.HasPrefix(c, "custom:") && len(c) > len("custom:") {
			add(c[len("custom:"):])
		}
	}
	providersCfg := settings.Providers.Get(ctx)
	var maps []any
	for _, p := range candidates {
		maps = append(maps, dictOrNil(providersCfg[p])["models"])
	}
	for _, m := range maps {
		if v, ok := modelSupportsVisionOverride(m, model); ok {
			return v, true
		}
	}
	return false, false
}

// base_url or api_key of the active inference provider, runtime first then the model and providers settings of ctx
func resolveInferenceValue(ctx context.Context, env *Env, provider, key string, runtimeOK func(string) bool) string {
	ctx = envHomeCtx(ctx, env)
	var runtime string
	if key == "api_key" {
		raw := runtimeMainValue(env, "api_key")
		switch raw.(type) {
		case func() string, func() (string, error):
			return materializeProbeAPIKey(raw)
		}
		runtime = materializeProbeAPIKey(raw)
	} else {
		runtime = runtimeMainText(env, key)
	}
	if runtime != "" && runtimeOK(runtime) {
		return runtime
	}
	own := settings.ModelBaseURL
	if key == "api_key" {
		own = settings.ModelAPIKey
	}
	if v := strings.TrimSpace(own.Get(ctx)); v != "" {
		return v
	}
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, p := range []string{provider, strings.TrimSpace(settings.ModelProvider.Get(ctx))} {
		if p == "" {
			continue
		}
		add(p)
		if strings.HasPrefix(strings.ToLower(p), "custom:") {
			add(strings.SplitN(p, ":", 2)[1])
		} else {
			add("custom:" + p)
		}
	}
	providersCfg := settings.Providers.Get(ctx)
	var entries []map[string]any
	for _, n := range names {
		if e := dictOrNil(providersCfg[n]); e != nil {
			entries = append(entries, e)
		}
	}
	for _, e := range entries {
		if v := cleanStr(e[key]); v != "" {
			return v
		}
	}
	return ""
}

// Base URL of the active inference provider, the runtime value only when it belongs to the requested provider
func resolveInferenceBaseURL(ctx context.Context, env *Env, provider string) string {
	requested := strings.ToLower(strings.TrimSpace(provider))
	return resolveInferenceValue(ctx, env, provider, "base_url", func(string) bool {
		return requested == "" || requested == strings.ToLower(runtimeMainText(env, "provider"))
	})
}

// API key resolved like the base URL so the probed endpoint gets its own key
func resolveInferenceAPIKey(ctx context.Context, env *Env, provider string) string {
	return resolveInferenceValue(ctx, env, provider, "api_key", func(string) bool { return true })
}

// Reports whether the active provider likely fronts a local Ollama server
func shouldProbeOllamaVision(ctx context.Context, provider, baseURL, apiKey string) bool {
	if strings.ToLower(strings.TrimSpace(provider)) == "ollama" {
		return true
	}
	if baseURL == "" || VisionCapabilities == nil {
		return false
	}
	return VisionCapabilities.IsLocalEndpoint(baseURL) && VisionCapabilities.DetectLocalServerType(ctx, baseURL, apiKey) == "ollama"
}

// Reports whether the settings of ctx name a specific auxiliary.vision backend
func explicitAuxVisionOverride(ctx context.Context) bool {
	provider := strings.ToLower(strings.TrimSpace(settings.AuxiliaryVisionProvider.Get(ctx)))
	return !((provider == "" || provider == "auto") && strings.TrimSpace(settings.AuxiliaryVisionModel.Get(ctx)) == "" && strings.TrimSpace(settings.AuxiliaryVisionBaseUrl.Get(ctx)) == "")
}

// Managed local runtime verdict, the serving process is the authority on whether it can see
func probeManagedRuntime(ctx context.Context, env *Env, provider, model string) (bool, bool) {
	if !isManagedLocalProvider(provider, resolveInferenceBaseURL(ctx, env, provider)) {
		return false, false
	}
	if model == "" || VisionCapabilities == nil {
		return false, false
	}
	return VisionCapabilities.ManagedModelSupportsVision(ctx, model)
}

// models.dev catalog verdict with network allowed, Codex picker variants mapped to their base slug
func probeModelsDev(ctx context.Context, env *Env, provider, model string) (bool, bool) {
	if VisionCapabilities == nil {
		return false, false
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "openai-codex" {
		model = VisionCapabilities.StripCodexContextVariantSuffix(model)
	}
	return VisionCapabilities.CatalogSupportsVision(ctx, provider, model)
}

// Ollama api show verdict for local endpoints
func probeOllama(ctx context.Context, env *Env, provider, model string) (bool, bool) {
	baseURL := resolveInferenceBaseURL(ctx, env, provider)
	if baseURL == "" && strings.ToLower(strings.TrimSpace(provider)) == "ollama" {
		baseURL = "http://localhost:11434/v1"
	}
	apiKey := resolveInferenceAPIKey(ctx, env, provider)
	if !shouldProbeOllamaVision(ctx, provider, baseURL, apiKey) || VisionCapabilities == nil {
		return false, false
	}
	return VisionCapabilities.QueryOllamaSupportsVision(ctx, model, baseURL, apiKey)
}

// Capability probes after the config override in priority order
var visionProbes = []func(ctx context.Context, env *Env, provider, model string) (bool, bool){
	probeManagedRuntime, probeModelsDev, probeOllama,
}

// Vision capability from the config override then the probes, known false when nothing resolves it
func lookupSupportsVision(ctx context.Context, env *Env, provider, model, requestedProvider string) (bool, bool) {
	ctx = envHomeCtx(ctx, env)
	if requestedProvider == "" &&
		strings.ToLower(runtimeMainText(env, "provider")) == strings.ToLower(strings.TrimSpace(provider)) &&
		runtimeMainText(env, "model") == strings.TrimSpace(model) {
		requestedProvider = runtimeMainText(env, "requested_provider")
	}
	if v, ok := supportsVisionOverride(ctx, provider, model, requestedProvider); ok {
		return v, true
	}
	if provider == "" || model == "" {
		return false, false
	}
	for _, probe := range visionProbes {
		if v, ok := probe(ctx, env, provider, model); ok {
			return v, true
		}
	}
	return false, false
}

// LookupSupportsVision reports the vision verdict for a provider and model under the session config and whether it is known
func LookupSupportsVision(ctx context.Context, env *Env, provider, model string) (bool, bool) {
	return lookupSupportsVision(visionCtx(ctx, env), env, provider, model, "")
}

// DecideImageInputMode returns native or text for a provider and model under the session config
func DecideImageInputMode(ctx context.Context, env *Env, provider, model, requestedProvider string) string {
	return decideImageInputMode(visionCtx(ctx, env), env, provider, model, requestedProvider)
}

// Native or text for the turn, an explicit auxiliary.vision backend winning in auto mode
func decideImageInputMode(ctx context.Context, env *Env, provider, model, requestedProvider string) string {
	ctx = envHomeCtx(ctx, env)
	if mode := settings.ImageInput.Get(ctx); mode != "auto" {
		return mode
	}
	if explicitAuxVisionOverride(ctx) {
		return "text"
	}
	if v, ok := lookupSupportsVision(ctx, env, provider, model, requestedProvider); ok && v {
		return "native"
	}
	return "text"
}

// Providers whose tool results accept image content
var toolResultMediaProviders = setOf(
	"openrouter", "vertex", "bedrock", "anthropic-vertex", "google-vertex",
	"anthropic", "claude", "anthropic-direct",
	"openai", "openai-chat", "openai-codex", "azure-openai",
)

// Gemini providers, gated on model because only 3.x accepts multimodal function responses
var geminiProviders = setOf("google", "gemini", "google-gemini", "google-vertex-gemini")

// Profile flags for a provider name, found false without an installed capability source
func providerVisionProfile(provider string) (bool, bool, bool) {
	if VisionCapabilities == nil {
		return false, false, false
	}
	return VisionCapabilities.ProviderVisionProfile(provider)
}

// Reports whether the route or its aggregator targeted model rejects image parts in tool messages
func routedModelRejectsVisionToolMessages(provider, model string) bool {
	name := strings.ToLower(strings.TrimSpace(provider))
	if _, toolMsgs, found := providerVisionProfile(name); found && !toolMsgs {
		return true
	}
	if VisionCapabilities == nil || !VisionCapabilities.IsRoutingAggregator(name) {
		return false
	}
	target, _, sep := strings.Cut(strings.TrimSpace(model), "/")
	if !sep || target == "" {
		return false
	}
	_, toolMsgs, found := providerVisionProfile(strings.ToLower(strings.TrimSpace(target)))
	return found && !toolMsgs
}

// Hard veto when the profile declares images are rejected inside tool result messages
func profileRejectsToolMedia(provider, model string) bool {
	return routedModelRejectsVisionToolMessages(provider, model)
}

// Reports whether provider and model accept image content inside a tool result message
func supportsMediaInToolResults(provider, model string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" || profileRejectsToolMedia(p, model) {
		return false
	}
	if toolResultMediaProviders[p] {
		return true
	}
	if geminiProviders[p] {
		m := strings.ToLower(strings.TrimSpace(model))
		for _, tag := range []string{"gemini-3", "gemini-pro-3", "gemini-flash-3"} {
			if strings.Contains(m, tag) {
				return true
			}
		}
		return false
	}
	vision, _, found := providerVisionProfile(p)
	return found && vision
}

// One gate for every native lane, the profile veto first then provider media support or a vision capable model
func acceptsToolResultImages(ctx context.Context, env *Env, provider, model string) bool {
	if profileRejectsToolMedia(provider, model) {
		return false
	}
	if supportsMediaInToolResults(provider, model) {
		return true
	}
	v, ok := lookupSupportsVision(ctx, env, provider, model, "")
	return ok && v
}

// NativeToolResultImages reports whether a tool may hand the main model pixels inside its result
func NativeToolResultImages(ctx context.Context, env *Env, provider, model string) bool {
	return nativeToolResultImages(visionCtx(ctx, env), env, provider, model)
}

// Image routing resolves to native and the route accepts images inside tool results
func nativeToolResultImages(ctx context.Context, env *Env, provider, model string) bool {
	return decideImageInputMode(ctx, env, provider, model, "") == "native" && acceptsToolResultImages(ctx, env, provider, model)
}

// ShouldUseNativeVisionFastPath applies the native image gate to the session main model
func ShouldUseNativeVisionFastPath(ctx context.Context, env *Env) bool {
	return NativeToolResultImages(ctx, env, readMainProvider(env), readMainModel(env))
}
