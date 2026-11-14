package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	openRouterBaseURL         = "https://openrouter.ai/api/v1"
	anthropicDefaultBaseURL   = "https://api.anthropic.com"
	noKeyPlaceholder          = "no-key-required"
	noAnthropicCredentialsMsg = "No Anthropic credentials found. Run 'icebath auth add anthropic' to sign in, or set ANTHROPIC_TOKEN / ANTHROPIC_API_KEY."
	envInferenceProvider      = "ICEBATH_INFERENCE_PROVIDER"
	vertexUnresolvedMsg       = "Vertex AI credentials could not be resolved. Vertex uses OAuth2 (not a static API key): provide a " +
		"service-account JSON via GOOGLE_APPLICATION_CREDENTIALS (or VERTEX_CREDENTIALS_PATH) in ~/.icebath/.env, " +
		"or run 'gcloud auth application-default login' for ADC. Set the GCP project/region under vertex: in " +
		"config.yaml if they aren't embedded in the credentials. Run `icebath setup` to install Vertex support."
)

var (
	vertexNames            = []string{"vertex", "google-vertex", "vertex-ai", "gcp-vertex", "vertexai"}
	localBypassCloudHosts  = []string{"openrouter.ai", "anthropic.com", "openai.com"}
	bedrockExplicitNames   = []string{"bedrock", "aws", "aws-bedrock", "amazon-bedrock", "amazon"}
	trailingV1             = regexp.MustCompile(`/v1/?$`)
	anthropicBedrockModel  = regexp.MustCompile(`(?i)^(?:(?:global|us|eu|apac|ap|au|jp|ca|sa|me|af)\.)?anthropic\.claude`)
	openAIRuntimeProviders = []string{"openai", "openai-codex"}
)

type resolution struct {
	r   *Router
	ctx context.Context
	cfg *config.Config
	mc  *modelCfg
}

type modelCfg struct {
	Default       string
	Provider      string
	BaseURL       string
	APIKey        string
	API           string
	KeyEnv        string
	APIKeyEnv     string
	APIMode       string
	AuthMode      string
	Entra         *config.EntraSettings
	OpenAIRuntime string
}

func (r *Router) begin(ctx context.Context) (*resolution, error) {
	x := &resolution{r: r, ctx: r.bind(ctx)}
	if r.frozen != nil {
		x.cfg = r.frozen.cfg
		return x, nil
	}
	cfg, err := r.c.o.Config.LoadReadonly(x.ctx)
	if err != nil {
		return nil, err
	}
	x.cfg = cfg
	return x, nil
}

func (x *resolution) o() *Options { return &x.r.c.o }

func (x *resolution) secret(name, def string) (string, error) {
	return x.o().Secrets.Get(x.ctx, name, def)
}

func (x *resolution) secretTrim(name string) (string, error) {
	v, err := x.secret(name, "")
	return strings.TrimSpace(v), err
}

func (x *resolution) liveConfig() (*config.Config, error) {
	return x.o().Config.LoadReadonly(x.ctx)
}

func (x *resolution) customEndpoints() ([]config.CustomEndpoint, error) {
	if x.r.frozen != nil {
		return x.r.frozen.custom, nil
	}
	return x.o().Config.CustomEndpoints(x.ctx)
}

func (x *resolution) frozenEntries() []auth.CustomEntry {
	if x.r.frozen != nil {
		return x.r.frozen.entries
	}
	return nil
}

func (x *resolution) model() *modelCfg {
	if x.mc != nil {
		return x.mc
	}
	m := x.cfg.Model
	mc := &modelCfg{}
	if m.Bare {
		mc.Default = strings.TrimSpace(m.Default.ID)
		x.mc = mc
		return mc
	}
	mc.Default, mc.Provider, mc.BaseURL, mc.APIKey, mc.API = m.Default.ID, m.Provider, m.BaseURL, m.APIKey, m.API
	mc.KeyEnv, mc.APIKeyEnv, mc.APIMode, mc.AuthMode = m.KeyEnv, m.APIKeyEnv, m.APIMode, m.AuthMode
	mc.Entra, mc.OpenAIRuntime = m.Entra, m.OpenAIRuntime
	if m.Default.Mapping {
		provider := m.Default.Provider
		if provider == "" {
			provider = m.Provider
		}
		if provider != "" && mc.Provider == "" {
			mc.Provider = provider
		}
	}
	base := strings.TrimSpace(mc.BaseURL)
	if strings.TrimSpace(mc.Default) == "" && base != "" {
		if host := providers.URLHostname(base); host == "localhost" || host == "127.0.0.1" {
			if detected := x.autoDetectLocalModel(base); detected != "" {
				mc.Default = detected
			}
		}
	}
	x.mc = mc
	return mc
}

func (x *resolution) autoDetectLocalModel(baseURL string) string {
	url := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(url, "/v1") {
		url += "/v1"
	}
	ctx, cancel := context.WithTimeout(x.ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/models", nil)
	if err != nil {
		slog.Debug("Auto-detect model failed", "base_url", baseURL, "error", err)
		return ""
	}
	resp, err := x.o().HTTP.Client().Do(req)
	if err != nil {
		slog.Debug("Auto-detect model failed", "base_url", baseURL, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return ""
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		slog.Debug("Auto-detect model failed", "base_url", baseURL, "error", err)
		return ""
	}
	if len(body.Data) == 1 && body.Data[0].ID != "" {
		return body.Data[0].ID
	}
	return ""
}

func cfgProvider(mc *modelCfg) string { return strings.ToLower(strings.TrimSpace(mc.Provider)) }

func effectiveModel(mc *modelCfg, target string) string {
	if target != "" {
		return target
	}
	return mc.Default
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func trimURL(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }

type explicitCreds struct {
	raw  bool
	key  auth.Credential
	text string
}

func explicitOf(t Target) explicitCreds {
	e := explicitCreds{raw: t.APIKey.Reveal() != "" || t.Tokens != nil}
	if text := strings.TrimSpace(t.APIKey.Reveal()); text != "" {
		e.key, e.text = staticKey(text, ""), text
	} else if t.Tokens != nil {
		e.key = tokenKey(t.Tokens, "")
	}
	return e
}

func (r *Router) Resolve(ctx context.Context, t Target) (*Route, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	return x.resolve(t)
}

func (r *Router) ResolveRoute(ctx context.Context, provider, model string) (catalog.Route, error) {
	rt, err := r.Resolve(ctx, Target{Provider: provider, Model: model})
	if err != nil {
		return catalog.Route{}, err
	}
	return catalog.Route{Provider: rt.Provider, BaseURL: rt.BaseURL, Key: rt.Credential}, nil
}

func (r *Router) ResolveRequested(ctx context.Context, requested string) (string, error) {
	x, err := r.begin(ctx)
	if err != nil {
		return "", err
	}
	return x.requestedProvider(requested)
}

func (x *resolution) requestedProvider(requested string) (string, error) {
	if v := strings.TrimSpace(requested); v != "" {
		return strings.ToLower(v), nil
	}
	if v := strings.TrimSpace(x.model().Provider); v != "" {
		return strings.ToLower(v), nil
	}
	var v string
	if env, ok := x.o().ClientEnv(x.ctx); ok {
		v = env[envInferenceProvider]
	} else {
		var err error
		if v, err = x.secret(envInferenceProvider, ""); err != nil {
			return "", err
		}
	}
	if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
		return v, nil
	}
	return "auto", nil
}

func (x *resolution) resolve(t Target) (*Route, error) {
	requested, err := x.requestedProvider(t.Provider)
	if err != nil {
		return nil, err
	}
	x.o().RecoverProvider(x.ctx, requested)
	if err := x.raiseIfDisabled(requested); err != nil {
		return nil, err
	}
	alias := requested
	requested, t.BaseURL, err = x.expandDirectAlias(requested, t.BaseURL)
	if err != nil {
		return nil, err
	}
	if err := x.raiseIfLocalAliasMissingEndpoint(requested, t.BaseURL); err != nil {
		return nil, err
	}
	rt, err := x.ladder(requested, t)
	if err != nil {
		return nil, err
	}
	if requested == "custom" && rt.Provider == "custom" && !keyPresent(rt.Credential) {
		return nil, authError(fmt.Sprintf("provider '%s' resolved without credentials (no endpoint or API key configured). "+
			"If this is a named custom provider, use its real name (see providers: in config.yaml).", requested), requested, "missing_api_key")
	}
	mode := x.codexAppServer(rt.Provider, rt.Mode, x.model(), alias)
	if mode != rt.Mode {
		slog.Info(fmt.Sprintf("model.openai_runtime=codex_app_server overrides the %s runtime (source=%s); its credential/endpoint "+
			"is not used — the app-server authenticates with its own login", rt.Provider, rt.Source))
	}
	rt.Mode = mode
	if err := x.raiseIfDeclaredKeyMissing(requested, rt, explicitOf(t)); err != nil {
		return nil, err
	}
	return rt, nil
}

func providerEnabled(e config.ProviderEntry) bool {
	s := e.Enabled
	if s.IsZero() {
		return true
	}
	switch s.Kind() {
	case config.KindBool:
		v, _ := s.AsBool()
		return v
	case config.KindString:
		switch strings.ToLower(strings.TrimSpace(s.String())) {
		case "false", "0", "no", "off":
			return false
		}
		return true
	case config.KindInt, config.KindFloat:
		f, _ := s.AsFloat()
		return f != 0
	}
	return false
}

func (x *resolution) raiseIfDisabled(requested string) error {
	if entry, ok := x.cfg.Providers.Get(requested); ok && entry.Valid() && !providerEnabled(entry) {
		return &ProviderDisabledError{Provider: requested}
	}
	return nil
}

func (x *resolution) ladder(requested string, t Target) (*Route, error) {
	ek := explicitOf(t)
	if rt, err := x.shortcuts(requested, t, ek); err != nil || rt != nil {
		return rt, err
	}
	if rt, err := x.namedCustomRung(requested, t, ek); err != nil || rt != nil {
		return rt, err
	}
	if t.BaseURL == "" && !ek.raw {
		if rt, err := x.localEndpointBypass(requested, t, ek); err != nil || rt != nil {
			return rt, err
		}
	}
	provider, err := x.resolveProvider(ProviderQuery{Requested: requested, ExplicitKey: ek.raw, ExplicitBaseURL: t.BaseURL != ""})
	if err != nil {
		return nil, err
	}
	mc := x.model()
	if rt, err := x.explicitRuntime(provider, requested, mc, ek, t); err != nil || rt != nil {
		return rt, err
	}
	if rt, err := x.fromPool(provider, requested, mc, ek, t); err != nil || rt != nil {
		return rt, err
	}
	var swallowed error
	if spec, ok := oauthSpecs[provider]; ok {
		rt, err := x.oauthRuntime(spec, provider, requested, mc, t.Model)
		if err == nil {
			return rt, nil
		}
		if !IsAuthError(err) || requested != "auto" {
			return nil, err
		}
		slog.Info(spec.failure + "; falling through to next provider.")
		swallowed = err
	}
	if provider == "minimax-oauth" {
		if rt, err := x.miniMaxOAuth(provider, requested); err != nil || rt != nil {
			return rt, err
		}
	}
	if x.isExternalProcess(provider) {
		return x.externalProcess(provider, requested)
	}
	if provider == "anthropic" {
		return x.anthropicEnv(requested, mc, t.Model)
	}
	if provider == "bedrock" {
		return x.bedrock(requested, mc, t.Model)
	}
	if row, ok := x.o().Providers.RegistryRow(x.ctx, provider); ok && row.AuthType == "api_key" {
		return x.apiKeyProvider(provider, row, requested, mc, t.Model)
	}
	fb, err := x.openRouterFallback(requested, ek, t.BaseURL)
	if err != nil {
		return nil, err
	}
	if swallowed != nil && !keyPresent(fb.Credential) {
		fb.AuthErr = swallowed
	}
	return fb, nil
}

func (x *resolution) route(provider string, mode wire.Mode, baseURL string, cred auth.Credential, source, requested string) *Route {
	if x.o().Providers.IsActualRoute(x.ctx, provider, baseURL) {
		mode = wire.ModeChat
		baseURL = auth.NormalizeActualBaseURL(baseURL)
	}
	cred.Source = source
	return &Route{Provider: provider, Mode: mode, BaseURL: baseURL, Credential: cred, Source: source, Requested: requested}
}

func (x *resolution) shortcuts(requested string, t Target, ek explicitCreds) (*Route, error) {
	if requested == "moa" {
		return x.route("moa", wire.ModeChat, "moa://local", staticKey("moa-virtual-provider", ""), "moa-virtual-provider", requested), nil
	}
	eff := strings.TrimSpace(t.BaseURL)
	if requested == "anthropic" && providers.HostMatches(eff, "azure.com") {
		key := ek.key
		if ek.text == "" {
			text, err := x.azureAnthropicEnvKey(&modelCfg{})
			if err != nil {
				return nil, err
			}
			key = staticKey(text, "")
		}
		return x.route("anthropic", wire.ModeAnthropic, strings.TrimRight(eff, "/"), key, "azure-explicit", requested), nil
	}
	if requested == "azure-foundry" {
		return x.azureFoundry(requested, x.model(), t, ek)
	}
	if slices.Contains(vertexNames, requested) {
		return x.vertex(requested)
	}
	return nil, nil
}

func (x *resolution) localEndpointBypass(requested string, t Target, ek explicitCreds) (*Route, error) {
	mc := x.model()
	base := strings.TrimSpace(mc.BaseURL)
	if base == "" || (cfgProvider(mc) != "auto" && cfgProvider(mc) != "") {
		return nil, nil
	}
	for _, host := range localBypassCloudHosts {
		if providers.HostMatches(base, host) {
			return nil, nil
		}
	}
	return x.openRouterFallback(requested, ek, t.BaseURL)
}

func (x *resolution) namedCustomRung(requested string, t Target, ek explicitCreds) (*Route, error) {
	rt, err := x.namedCustomRuntime(requested, ek, t.BaseURL, t.Model)
	if err != nil || rt == nil {
		return nil, err
	}
	rt.Requested = requested
	if rt.Provider == "custom" {
		mode := rt.Mode
		if mode == "" {
			mode = wire.ModeChat
		}
		rt.Mode = x.codexAppServer("custom", mode, x.model(), requested)
	}
	return rt, nil
}

func (x *resolution) openRouterFallback(requested string, ek explicitCreds, explicitBase string) (*Route, error) {
	rt, err := x.openRouterRuntime(requested, ek, explicitBase)
	if err != nil {
		return nil, err
	}
	rt.Requested = requested
	return rt, nil
}

func (x *resolution) loopback(host string) bool {
	switch strings.TrimRight(strings.ToLower(host), ".") {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

func (x *resolution) resolvesToCustom(name string) bool {
	if canonical, known := x.canonicalName(name); known || canonical != "auto" {
		return known && canonical == "custom"
	}
	p, err := x.resolveProvider(ProviderQuery{Requested: name})
	return err == nil && p == "custom"
}

func (x *resolution) trustworthyForBareCustom(cfgBase, provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	bu := strings.TrimSpace(cfgBase)
	if bu == "" {
		return false
	}
	return p == "custom" || (p != "" && p != "auto" && x.resolvesToCustom(p)) ||
		(!providers.HostMatches(bu, "openrouter.ai") && x.loopback(providers.URLHostname(bu)))
}

func (x *resolution) raiseIfLocalAliasMissingEndpoint(requested, explicitBase string) error {
	norm := strings.ToLower(strings.TrimSpace(requested))
	if norm == "" || norm == "custom" || slices.Contains(catalog.LlamaCppAliases, norm) || !x.resolvesToCustom(norm) {
		return nil
	}
	if strings.TrimSpace(explicitBase) != "" {
		return nil
	}
	if v, err := x.secretTrim("CUSTOM_BASE_URL"); err != nil || v != "" {
		return err
	}
	mc := x.model()
	if x.trustworthyForBareCustom(mc.BaseURL, cfgProvider(mc)) {
		return nil
	}
	cp, err := x.namedCustomProvider(requested, nil, true)
	if err != nil {
		return err
	}
	if cp != nil && strings.TrimSpace(cp.BaseURL) != "" {
		return nil
	}
	return authError(fmt.Sprintf("provider '%s' has no endpoint configured, so the request is not sent anywhere "+
		"(it would otherwise fall back to OpenRouter). Set providers.%s.base_url or model.base_url in config.yaml.", requested, norm),
		requested, "missing_base_url")
}

func (x *resolution) configBaseURLFor(mc *modelCfg, provider string) string {
	configured := cfgProvider(mc)
	if provider == "actual" {
		configured = modelsProvider(configured)
	}
	if x.sameRegistered(provider, configured) {
		return trimURL(mc.BaseURL)
	}
	return ""
}

func modelsProvider(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		n = "openrouter"
	}
	if alias, ok := auth.ProviderAlias(n); ok {
		return alias
	}
	return n
}

func (x *resolution) anthropicOverrideOK(baseURL string) bool {
	candidate := strings.TrimSpace(baseURL)
	if candidate == "" {
		return false
	}
	host := strings.ToLower(providers.URLHostname(candidate))
	if host == "" {
		return false
	}
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com") || strings.HasSuffix(host, ".claude.com") ||
		strings.HasSuffix(host, ".azure.com") || detectModeForURL(candidate) == wire.ModeAnthropic
}

func (x *resolution) anthropicCfgBaseURL(mc *modelCfg) string {
	base := x.configBaseURLFor(mc, "anthropic")
	if x.anthropicOverrideOK(base) {
		return base
	}
	return ""
}

func (x *resolution) anthropicTokenOrRaise(model string) (auth.Credential, error) {
	store := x.o().Auth
	if token, ok := store.ResolveAnthropicToken(x.ctx, model); ok && !token.Empty() {
		return auth.Credential{Kind: auth.CredAPIKey, Key: token}, nil
	}
	if model != "" {
		if token, ok := store.ResolveAnthropicToken(x.ctx, ""); ok && !token.Empty() {
			return auth.Credential{}, authError(fmt.Sprintf("Anthropic credentials are rate-limited for %s; "+
				"other Claude models remain available (see `icebath auth list`).", model), "", "")
		}
	}
	return auth.Credential{}, authError(noAnthropicCredentialsMsg, "", "")
}

func (x *resolution) hostDerivedAPIKey(baseURL string) (string, error) {
	host := providers.URLHostname(baseURL)
	if host == "" || host == "localhost" || strings.Contains(host, ":") {
		return "", nil
	}
	parts := strings.Split(host, ".")
	if strings.ContainsAny(parts[len(parts)-1], "0123456789") {
		return "", nil
	}
	var labels []string
	for _, l := range parts {
		if l != "" {
			labels = append(labels, l)
		}
	}
	for len(labels) > 0 && (labels[0] == "api" || labels[0] == "www") {
		labels = labels[1:]
	}
	if len(labels) < 2 {
		return "", nil
	}
	var b strings.Builder
	for _, r := range labels[len(labels)-2] {
		if isAlnum(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	sanitized := strings.ToUpper(b.String())
	if sanitized == "" || !isAlpha([]rune(sanitized)[0]) || sanitized == "OPENAI" || sanitized == "OPENROUTER" || sanitized == "OLLAMA" {
		return "", nil
	}
	return x.secretTrim(sanitized + "_API_KEY")
}

func (x *resolution) hostGatedCandidates(baseURL string, ollama bool) ([]auth.Credential, error) {
	isOpenAI := providers.HostMatches(baseURL, "openai.com") || providers.HostMatches(baseURL, "openai.azure.com")
	envOpenAIBase, err := x.secretTrim("OPENAI_BASE_URL")
	if err != nil {
		return nil, err
	}
	envOpenAIBase = strings.TrimRight(envOpenAIBase, "/")
	isOpenAI = isOpenAI || (envOpenAIBase != "" && trimURL(baseURL) == envOpenAIBase)
	var out []auth.Credential
	read := func(cond bool, name string) error {
		if !cond {
			out = append(out, noKey(""))
			return nil
		}
		v, err := x.secretTrim(name)
		out = append(out, staticKey(v, ""))
		return err
	}
	if ollama {
		if err := read(providers.HostMatches(baseURL, "ollama.com"), "OLLAMA_API_KEY"); err != nil {
			return nil, err
		}
	}
	if err := read(isOpenAI, "OPENAI_API_KEY"); err != nil {
		return nil, err
	}
	if err := read(providers.HostMatches(baseURL, "openrouter.ai"), "OPENROUTER_API_KEY"); err != nil {
		return nil, err
	}
	derived, err := x.hostDerivedAPIKey(baseURL)
	if err != nil {
		return nil, err
	}
	return append(out, staticKey(derived, "")), nil
}

func firstUsable(cands []auth.Credential) auth.Credential {
	for _, c := range cands {
		if usableKey(c) {
			return staticKey(strings.TrimSpace(c.Key.Reveal()), "")
		}
	}
	return noKey("")
}

func portalMinKeyTTL() time.Duration {
	seconds := 1800
	if raw := strings.TrimSpace(os.Getenv("ICEBATH_PORTAL_MIN_KEY_TTL_SECONDS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			seconds = v
		}
	}
	return time.Duration(max(60, seconds)) * time.Second
}

func (x *resolution) portalInferenceOverride() string {
	v, ok, err := x.o().Secrets.Lookup(x.ctx, portal.EnvInferenceBaseURL)
	if err != nil {
		slog.Warn(fmt.Sprintf("model: %s unreadable — no profile secret scope on a multiplexed call; treating the "+
			"override as absent (default routing applies). The caller needs a profile scope binding.", portal.EnvInferenceBaseURL))
		return ""
	}
	if !ok {
		return ""
	}
	return auth.OptionalBaseURL(v)
}

func (x *resolution) portalCreds() (portal.Credentials, error) {
	raw, err := x.secret(portal.EnvTimeoutSeconds, "15")
	if err != nil {
		return portal.Credentials{}, err
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return portal.Credentials{}, fmt.Errorf("model: %s is not a number: %w", portal.EnvTimeoutSeconds, err)
	}
	return x.o().Portal.RuntimeCredentials(x.ctx, portal.ResolveOptions{Timeout: time.Duration(seconds * float64(time.Second))})
}

func (x *resolution) portalMode(model string) (wire.Mode, error) {
	live, err := x.liveConfig()
	if err != nil {
		return "", err
	}
	return providers.PortalMode(model, providers.PortalAnthropicWire(live)), nil
}

func (x *resolution) finalizeBaseURL(provider string, mode wire.Mode, baseURL string) string {
	reg := x.o().Providers
	if _, ok := reg.OpenCodeFamily(x.ctx, provider); ok {
		baseURL = reg.NormalizeOpenCodeBaseURL(x.ctx, provider, mode, baseURL)
	}
	if provider == "lmstudio" {
		baseURL = providers.NormalizeLMStudioBaseURL(baseURL)
	}
	if provider == "actual" {
		baseURL = auth.NormalizeActualBaseURL(baseURL)
	}
	return baseURL
}

func runtimeCred(rc auth.RuntimeCredentials) auth.Credential {
	if rc.Tokens != nil {
		return tokenKey(rc.Tokens, "")
	}
	return staticKey(rc.APIKey.Reveal(), "")
}

func isoTime(v string) time.Time {
	t, _ := auth.ParseISOTimestamp(v)
	return t
}

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (x *resolution) explicitRuntime(provider, requested string, mc *modelCfg, ek explicitCreds, t Target) (*Route, error) {
	base := trimURL(t.BaseURL)
	if !keyPresent(ek.key) && base == "" {
		return nil, nil
	}
	switch provider {
	case "anthropic":
		if base == "" {
			base = firstOf(x.anthropicCfgBaseURL(mc), anthropicDefaultBaseURL)
		}
		key := ek.key
		if !keyPresent(key) {
			var err error
			if key, err = x.anthropicTokenOrRaise(t.Model); err != nil {
				return nil, err
			}
		}
		return x.route("anthropic", wire.ModeAnthropic, base, key, "explicit", requested), nil
	case auth.ProviderCodex:
		key, url, rc := ek.key, firstOf(base, auth.DefaultCodexBaseURL), auth.RuntimeCredentials{}
		if !keyPresent(key) {
			var err error
			if rc, err = x.o().Auth.ResolveCodex(x.ctx, auth.ResolveOptions{}); err != nil {
				return nil, err
			}
			key, url = runtimeCred(rc), firstOf(base, strings.TrimRight(rc.BaseURL, "/"), url)
		}
		rt := x.route(auth.ProviderCodex, wire.ModeResponses, url, key, "explicit", requested)
		rt.LastRefresh = isoTime(rc.LastRefresh)
		return rt, nil
	case "qwen-oauth":
		key, url, rc := ek.key, firstOf(base, qwenBaseURL()), auth.RuntimeCredentials{}
		if !keyPresent(key) {
			var err error
			if rc, err = x.o().Auth.ResolveQwen(x.ctx, auth.ResolveOptions{}); err != nil {
				return nil, err
			}
			key, url = runtimeCred(rc), firstOf(base, strings.TrimRight(rc.BaseURL, "/"), url)
		}
		rt := x.route("qwen-oauth", wire.ModeChat, url, key, "explicit", requested)
		rt.ExpiresAt = msTime(rc.ExpiresAtMS)
		return rt, nil
	case portal.Provider:
		return x.explicitPortal(requested, mc, ek, base, t.Model)
	case "azure-foundry":
		return x.azureFoundry(requested, mc, Target{BaseURL: base, APIKey: auth.NewSecret(ek.text)}, ek)
	}
	row, ok := x.o().Providers.RegistryRow(x.ctx, provider)
	if !ok || row.AuthType != "api_key" {
		return nil, nil
	}
	return x.explicitAPIKeyProvider(provider, row, requested, mc, ek.key, base, t.Model)
}

func qwenBaseURL() string {
	return firstOf(strings.TrimRight(strings.TrimSpace(os.Getenv("ICEBATH_QWEN_BASE_URL")), "/"), auth.DefaultQwenBaseURL)
}

func stateText(state json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(state, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}

func (x *resolution) explicitPortal(requested string, mc *modelCfg, ek explicitCreds, base, target string) (*Route, error) {
	state, _, err := x.o().Auth.State(x.ctx, portal.Provider)
	if err != nil {
		return nil, err
	}
	url := base
	if url == "" {
		url = x.portalInferenceOverride()
	}
	if url == "" {
		url = trimURL(firstOf(stateText(state, "inference_base_url"), auth.DefaultPortalInferenceURL))
	}
	key := ek.key
	if !keyPresent(key) {
		agentKey := stateText(state, "agent_key")
		if agentKeyUsable(agentKey, stateText(state, "scope"), stateText(state, "agent_key_expires_at"), portalMinKeyTTL()) {
			key = staticKey(strings.TrimSpace(agentKey), "")
		}
	}
	expiry := firstOf(stateText(state, "agent_key_expires_at"), stateText(state, "expires_at"))
	if !keyPresent(key) {
		creds, err := x.portalCreds()
		if err != nil {
			return nil, err
		}
		key, expiry = staticKey(creds.APIKey.Reveal(), ""), creds.ExpiresAt
		url = firstOf(base, strings.TrimRight(creds.BaseURL, "/"), url)
	}
	mode, err := x.portalMode(effectiveModel(mc, target))
	if err != nil {
		return nil, err
	}
	rt := x.route(portal.Provider, mode, url, key, "explicit", requested)
	rt.ExpiresAt = isoTime(expiry)
	return rt, nil
}

func (x *resolution) actualURL(provider, baseURL string) string {
	if provider == "actual" {
		return auth.NormalizeActualBaseURL(baseURL)
	}
	return baseURL
}

func (x *resolution) actualLocalKey(provider string, key auth.Credential, baseURL string) auth.Credential {
	if provider == "actual" && !keyPresent(key) && auth.IsActualLocalBaseURL(baseURL) {
		return staticKey(auth.ActualLocalNoAuthPlaceholder, "")
	}
	return key
}

func (x *resolution) explicitAPIKeyProvider(provider string, row providers.Row, requested string, mc *modelCfg, key auth.Credential, base, target string) (*Route, error) {
	store := x.o().Auth
	if base == "" {
		switch provider {
		case "actual":
			base = x.configBaseURLFor(mc, provider)
			if base == "" {
				creds, err := store.ResolveAPIKey(x.ctx, provider)
				if err != nil {
					return nil, err
				}
				base = creds.BaseURL
			}
		case "kimi-coding", "kimi-coding-cn":
			creds, err := store.ResolveAPIKey(x.ctx, provider)
			if err != nil {
				return nil, err
			}
			base = strings.TrimRight(creds.BaseURL, "/")
		default:
			envURL := ""
			if row.BaseURLEnvVar != "" {
				v, err := x.secretTrim(row.BaseURLEnvVar)
				if err != nil {
					return nil, err
				}
				envURL = strings.TrimRight(v, "/")
			}
			base = firstOf(envURL, row.InferenceBaseURL)
		}
	}
	base = x.actualURL(provider, base)
	if !keyPresent(key) {
		creds, err := store.ResolveAPIKey(x.ctx, provider)
		if err != nil {
			return nil, err
		}
		key = staticKey(creds.APIKey.Reveal(), "")
		if base == "" {
			base = x.actualURL(provider, strings.TrimRight(creds.BaseURL, "/"))
		}
	}
	mode := x.apiKeyProviderMode(provider, mc, keyText(key), base, firstOf(target, mc.Default), true)
	base = x.finalizeBaseURL(provider, mode, base)
	key = x.actualLocalKey(provider, key, base)
	return x.route(provider, mode, strings.TrimRight(base, "/"), key, "explicit", requested), nil
}

func (x *resolution) openRouterShouldUsePool(requested string, mc *modelCfg, ek explicitCreds, explicitBase string) (bool, error) {
	cfgBase := strings.TrimSpace(mc.BaseURL)
	envBase, err := x.secretTrim("OPENAI_BASE_URL")
	if err != nil {
		return false, err
	}
	if envBase == "" {
		if envBase, err = x.secretTrim("OPENROUTER_BASE_URL"); err != nil {
			return false, err
		}
	}
	p := cfgProvider(mc)
	mirror := cfgBase != "" && (p == "auto" || p == "custom" || (p == "openrouter" && !providers.HostMatches(cfgBase, "openrouter.ai")))
	custom := explicitBase != "" || envBase != "" || mirror
	return (requested == "openrouter" || requested == "auto") && !custom && !(ek.raw || explicitBase != ""), nil
}

func (x *resolution) fromPool(provider, requested string, mc *modelCfg, ek explicitCreds, t Target) (*Route, error) {
	use := provider != "openrouter"
	if !use {
		var err error
		if use, err = x.openRouterShouldUsePool(requested, mc, ek, t.BaseURL); err != nil {
			return nil, err
		}
	}
	if !use {
		return nil, nil
	}
	store := x.o().Auth
	pool, err := store.Pool(x.ctx, provider)
	if err != nil {
		slog.Debug("credential pool unavailable", "provider", provider, "error", err)
		return nil, nil
	}
	if pool == nil || !pool.HasCredentials() {
		return nil, nil
	}
	entry, ok, err := pool.Select(x.ctx, t.Model)
	if err != nil || !ok {
		return nil, err
	}
	key := entry.RuntimeAPIKey()
	if provider == portal.Provider {
		entry, key = x.refreshPortalPoolEntry(pool, entry, key)
	}
	if !auth.HasUsableSecret(key.Reveal(), 4) {
		return nil, nil
	}
	if !store.PoolMatchesProvider(x.ctx, pool.Provider(), provider, entry.RuntimeBaseURL(x.ctx)) {
		return nil, nil
	}
	return x.runtimeFromPoolEntry(provider, entry, key, requested, mc, pool, t.Model)
}

func (x *resolution) refreshPortalPoolEntry(pool *auth.Pool, entry auth.Entry, key auth.Secret) (auth.Entry, auth.Secret) {
	ttl := portalMinKeyTTL()
	usable := func(e auth.Entry) bool {
		return agentKeyUsable(e.AgentKey.Reveal(), e.ExtraString("scope"), e.AgentKeyExpiresAt, ttl)
	}
	if usable(entry) {
		return entry, key
	}
	slog.Debug("Icebath Portal pool entry agent_key expired/missing, refreshing selected pool entry")
	refreshed, ok, err := pool.TryRefreshCurrent(x.ctx)
	if err != nil {
		slog.Debug("Icebath Portal pool entry refresh failed", "error", err)
		ok = false
	}
	if ok {
		entry, key = refreshed, refreshed.RuntimeAPIKey()
	}
	if key.Empty() || !usable(entry) {
		slog.Debug("Icebath Portal pool entry agent_key still unavailable, falling through to runtime resolution")
		key = auth.Secret{}
	}
	return entry, key
}

func (x *resolution) runtimeFromPoolEntry(provider string, entry auth.Entry, key auth.Secret, requested string, mc *modelCfg, pool *auth.Pool, target string) (*Route, error) {
	mode, base, err := x.poolEntryModeAndURL(provider, entry, key, mc, effectiveModel(mc, target), strings.TrimRight(entry.RuntimeBaseURL(x.ctx), "/"))
	if err != nil {
		return nil, err
	}
	base = x.finalizeBaseURL(provider, mode, base)
	rt := x.route(provider, mode, base, staticKey(key.Reveal(), ""), entry.Source, requested)
	rt.Pool = pool
	return rt, nil
}

func (x *resolution) poolEntryModeAndURL(provider string, entry auth.Entry, key auth.Secret, mc *modelCfg, model, base string) (wire.Mode, string, error) {
	reg := x.o().Providers
	if provider == "actual" && strings.HasPrefix(entry.Source, "env:") {
		base = firstOf(x.configBaseURLFor(mc, provider), base)
	}
	simple := func(mode wire.Mode, def string) (wire.Mode, string, error) { return mode, firstOf(base, def), nil }
	switch provider {
	case auth.ProviderCodex:
		override, err := x.secretTrim("ICEBATH_CODEX_BASE_URL")
		if err != nil {
			return "", "", err
		}
		if override = strings.TrimRight(override, "/"); override != "" {
			return wire.ModeResponses, override, nil
		}
		if base == "" || base == auth.DefaultCodexBaseURL {
			base = firstOf(x.configBaseURLFor(mc, provider), base)
		}
		return simple(wire.ModeResponses, auth.DefaultCodexBaseURL)
	case "xai-oauth":
		return simple(wire.ModeResponses, auth.DefaultXAIOAuthBaseURL)
	case "qwen-oauth":
		return simple(wire.ModeChat, auth.DefaultQwenBaseURL)
	case "openrouter":
		return simple(wire.ModeChat, openRouterBaseURL)
	case "minimax-oauth":
		row, _ := reg.RegistryRow(x.ctx, "minimax-oauth")
		return simple(wire.ModeAnthropic, row.InferenceBaseURL)
	case "xai":
		row, _ := reg.RegistryRow(x.ctx, "xai")
		canonical := strings.TrimRight(row.InferenceBaseURL, "/")
		if b := strings.TrimRight(base, "/"); b == "" || b == canonical {
			base = firstOf(x.configBaseURLFor(mc, provider), base)
		}
		return simple(wire.ModeResponses, "")
	case "anthropic":
		return wire.ModeAnthropic, firstOf(x.anthropicCfgBaseURL(mc), base, anthropicDefaultBaseURL), nil
	case portal.Provider:
		mode, err := x.portalMode(model)
		if err != nil {
			return "", "", err
		}
		return mode, firstOf(x.portalInferenceOverride(), base), nil
	}
	if x.perModelProfile(provider) != nil {
		mode := x.profileRuntimeMode(provider, mc, key.Reveal(), model)
		if base == "" {
			row, _ := reg.RegistryRow(x.ctx, provider)
			base = row.InferenceBaseURL
		}
		return mode, base, nil
	}
	if provider == "azure-foundry" {
		mode := wire.ModeChat
		if cfgProvider(mc) == "azure-foundry" {
			base = firstOf(x.configBaseURLFor(mc, "azure-foundry"), base)
			mode = firstMode(x.parseMode(mc.APIMode), mode)
		}
		mode = azureInferredMode(model, mode)
		if mode == wire.ModeAnthropic {
			base = trailingV1.ReplaceAllString(base, "")
		}
		return mode, base, nil
	}
	if row, ok := reg.RegistryRow(x.ctx, provider); ok && (base == "" || strings.TrimRight(base, "/") == strings.TrimRight(row.InferenceBaseURL, "/")) {
		base = firstOf(x.configBaseURLFor(mc, provider), base, row.InferenceBaseURL)
	}
	return x.configuredOrFallbackMode(provider, mc, base, model, true), base, nil
}

type oauthSpec struct {
	mode       wire.Mode
	byModel    bool
	source     string
	failure    string
	defaultURL string
	resolve    func(x *resolution) (oauthCreds, error)
}

type oauthCreds struct {
	baseURL     string
	key         auth.Credential
	source      string
	expiresAt   time.Time
	lastRefresh time.Time
}

func authStoreCreds(resolve func(s *auth.Store, ctx context.Context) (auth.RuntimeCredentials, error)) func(x *resolution) (oauthCreds, error) {
	return func(x *resolution) (oauthCreds, error) {
		rc, err := resolve(x.o().Auth, x.ctx)
		if err != nil {
			return oauthCreds{}, err
		}
		return oauthCreds{baseURL: rc.BaseURL, key: runtimeCred(rc), source: rc.Source, expiresAt: msTime(rc.ExpiresAtMS), lastRefresh: isoTime(rc.LastRefresh)}, nil
	}
}

var oauthSpecs = map[string]oauthSpec{
	portal.Provider: {byModel: true, source: "portal", failure: "Auto-detected Icebath Portal provider but credentials failed",
		resolve: func(x *resolution) (oauthCreds, error) {
			c, err := x.portalCreds()
			if err != nil {
				return oauthCreds{}, err
			}
			return oauthCreds{baseURL: c.BaseURL, key: staticKey(c.APIKey.Reveal(), ""), source: c.Source, expiresAt: isoTime(c.ExpiresAt)}, nil
		}},
	auth.ProviderCodex: {mode: wire.ModeResponses, source: "icebath-auth-store", failure: "Auto-detected Codex provider but credentials failed",
		resolve: authStoreCreds(func(s *auth.Store, ctx context.Context) (auth.RuntimeCredentials, error) {
			return s.ResolveCodex(ctx, auth.ResolveOptions{})
		})},
	"xai-oauth": {mode: wire.ModeResponses, source: "icebath-auth-store", failure: "Auto-detected xAI OAuth provider but credentials failed",
		defaultURL: auth.DefaultXAIOAuthBaseURL,
		resolve: authStoreCreds(func(s *auth.Store, ctx context.Context) (auth.RuntimeCredentials, error) {
			return s.ResolveXAI(ctx, auth.ResolveOptions{})
		})},
	"qwen-oauth": {mode: wire.ModeChat, source: "qwen-cli", failure: "Qwen OAuth credentials failed",
		resolve: authStoreCreds(func(s *auth.Store, ctx context.Context) (auth.RuntimeCredentials, error) {
			return s.ResolveQwen(ctx, auth.ResolveOptions{})
		})},
}

func (x *resolution) oauthRuntime(spec oauthSpec, provider, requested string, mc *modelCfg, target string) (*Route, error) {
	creds, err := spec.resolve(x)
	if err != nil {
		return nil, err
	}
	mode := spec.mode
	if spec.byModel {
		if mode, err = x.portalMode(effectiveModel(mc, target)); err != nil {
			return nil, err
		}
	}
	rt := x.route(provider, mode, firstOf(strings.TrimRight(creds.baseURL, "/"), spec.defaultURL), creds.key, firstOf(creds.source, spec.source), requested)
	rt.ExpiresAt, rt.LastRefresh = creds.expiresAt, creds.lastRefresh
	return rt, nil
}

func (x *resolution) miniMaxOAuth(provider, requested string) (*Route, error) {
	row, ok := x.o().Providers.RegistryRow(x.ctx, provider)
	if !ok || row.AuthType != "oauth_minimax" {
		return nil, nil
	}
	rc, err := x.o().Auth.ResolveMiniMax(x.ctx, auth.ResolveOptions{})
	if err != nil {
		return nil, err
	}
	return x.route(provider, wire.ModeAnthropic, rc.BaseURL, runtimeCred(rc), firstOf(rc.Source, "oauth"), requested), nil
}

func (x *resolution) isExternalProcess(provider string) bool {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return false
	}
	if row, ok := x.o().Providers.RegistryRow(x.ctx, name); ok {
		return row.AuthType == "external_process"
	}
	p, ok := x.o().Providers.Get(x.ctx, name)
	return ok && p.Spec().AuthType == "external_process"
}

func (x *resolution) externalProcess(provider, requested string) (*Route, error) {
	creds, err := x.o().Auth.ResolveExternalProcess(x.ctx, provider)
	if err != nil {
		return nil, err
	}
	rt := x.route(provider, wire.ModeChat, strings.TrimRight(creds.BaseURL, "/"), staticKey(creds.APIKey.Reveal(), ""), firstOf(creds.Source, "process"), requested)
	rt.Command, rt.Args = creds.Command, slices.Clone(creds.Args)
	return rt, nil
}

func (x *resolution) azureAnthropicEnvKey(mc *modelCfg) (string, error) {
	for _, hint := range []string{mc.KeyEnv, mc.APIKeyEnv} {
		if name := strings.TrimSpace(hint); name != "" {
			v, err := x.secretTrim(name)
			if err != nil || v != "" {
				return v, err
			}
		}
	}
	if v := strings.TrimSpace(mc.APIKey); v != "" {
		return v, nil
	}
	v, err := x.secretTrim("AZURE_ANTHROPIC_KEY")
	if err != nil || v != "" {
		return v, err
	}
	return x.secretTrim("ANTHROPIC_API_KEY")
}

func (x *resolution) anthropicEnv(requested string, mc *modelCfg, target string) (*Route, error) {
	base := firstOf(x.anthropicCfgBaseURL(mc), anthropicDefaultBaseURL)
	var key auth.Credential
	if providers.HostMatches(base, "azure.com") {
		text, err := x.azureAnthropicEnvKey(mc)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, authError("No Azure Anthropic API key found. Set AZURE_ANTHROPIC_KEY or ANTHROPIC_API_KEY, or point "+
				"key_env/api_key_env in your config.yaml model section at a custom env var.", "", "")
		}
		key = staticKey(text, "")
	} else {
		var err error
		if key, err = x.anthropicTokenOrRaise(target); err != nil {
			return nil, err
		}
	}
	return x.route("anthropic", wire.ModeAnthropic, base, key, "env", requested), nil
}

func (x *resolution) bedrock(requested string, mc *modelCfg, target string) (*Route, error) {
	owner, ok := x.o().Providers.BedrockOwner()
	if !ok {
		return nil, errors.New("model: the Bedrock provider is not registered")
	}
	access, err := owner.Bedrock(x.ctx, true)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(bedrockExplicitNames, requested) && !access.HasCredentials(x.ctx) {
		return nil, authError("No AWS credentials found for Bedrock. Configure one of:\n"+
			"  - AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY\n"+
			"  - AWS_PROFILE (for SSO / named profiles)\n"+
			"  - IAM instance role (EC2, ECS, Lambda)\n"+
			"Or run 'aws configure' to set up credentials.", "", "no_aws_credentials")
	}
	live, err := x.liveConfig()
	if err != nil {
		return nil, err
	}
	region := access.RuntimeRegion()
	source := firstOf(access.Source(x.ctx), "aws-sdk-default-chain")
	guardrail := providers.BedrockGuardrail(live.Bedrock)
	model := strings.TrimSpace(firstOf(target, mc.Default))
	bearer := access.BearerToken()
	signed := auth.Credential{Kind: auth.CredSigned, Sign: access.Signer(region)}
	rt := x.route("bedrock", wire.ModeBedrock, "https://bedrock-runtime."+region+".amazonaws.com", signed, source, requested)
	rt.Region = region
	switch {
	case providers.BedrockOpenAIUsesMantle(model):
		rt.Mode, rt.BaseURL, rt.Model, rt.BedrockOpenAI = wire.ModeResponses, access.OpenAIBaseURL(region), model, true
		if !bearer.Empty() {
			rt.Source = "AWS_BEARER_TOKEN_BEDROCK"
			rt.Credential = staticKey(bearer.Reveal(), rt.Source)
		}
	case anthropicBedrockModel.MatchString(model) && bearer.Empty():
		rt.Mode, rt.BedrockAnthropic = wire.ModeAnthropic, true
		rt.Credential = auth.Credential{Kind: auth.CredSigned, Sign: access.SigV4(region), Source: source}
	}
	if guardrail != nil {
		rt.Guardrail = guardrail
	}
	return rt, nil
}

func (x *resolution) apiKeyProvider(provider string, row providers.Row, requested string, mc *modelCfg, target string) (*Route, error) {
	creds, err := x.o().Auth.ResolveAPIKey(x.ctx, provider)
	if err != nil {
		return nil, err
	}
	key, source := creds.APIKey.Reveal(), creds.Source
	if provider == "actual" && !auth.HasUsableSecret(key, 4) {
		candidate := firstOf(x.configBaseURLFor(mc, provider), strings.TrimRight(creds.BaseURL, "/"))
		if auth.IsActualLocalBaseURL(auth.NormalizeActualBaseURL(candidate)) {
			key, source = auth.ActualLocalNoAuthPlaceholder, firstOf(source, "local-offline")
		}
	}
	if !auth.HasUsableSecret(key, 4) {
		hint := ""
		if len(row.APIKeyEnvVars) > 0 {
			hint = " Set " + strings.Join(row.APIKeyEnvVars, ", ") + "."
		}
		hint += x.missingCredentialsSuffix(provider)
		named, err := x.namedProfileKeyHint(row.APIKeyEnvVars)
		if err != nil {
			return nil, err
		}
		return nil, authError(fmt.Sprintf("No usable credentials found for provider '%s'.%s%s", provider, hint, named), provider, "missing_api_key")
	}
	base := x.actualURL(provider, firstOf(x.configBaseURLFor(mc, provider), strings.TrimRight(creds.BaseURL, "/")))
	mode := x.apiKeyProviderMode(provider, mc, key, base, firstOf(target, mc.Default), true)
	base = x.finalizeBaseURL(provider, mode, base)
	cred := x.actualLocalKey(provider, staticKey(key, ""), base)
	return x.route(provider, mode, base, cred, firstOf(source, "env"), requested), nil
}

func (x *resolution) vertex(requested string) (*Route, error) {
	owner, ok := x.o().Providers.VertexOwner()
	if !ok {
		return nil, authError(vertexUnresolvedMsg, "", "")
	}
	v, err := owner.Vertex(x.ctx)
	if err != nil {
		return nil, &auth.Error{Message: vertexUnresolvedMsg, Cause: err}
	}
	var base string
	var cred auth.Credential
	if deferredMint(x.ctx) {
		if base, err = v.BaseURL(x.ctx, "", ""); err != nil || base == "" {
			return nil, &auth.Error{Message: vertexUnresolvedMsg, Cause: err}
		}
		cred = tokenKey(v.TokenSource(), "")
	} else {
		var tok auth.Token
		if tok, base, err = v.Config(x.ctx, "", ""); err != nil || tok.Value.Empty() || base == "" {
			return nil, &auth.Error{Message: vertexUnresolvedMsg, Cause: err}
		}
		cred = staticKey(tok.Value.Reveal(), "")
		cred.ExpiresAt = tok.Expiry
	}
	return x.route("vertex", wire.ModeChat, strings.TrimRight(base, "/"), cred, "vertex-oauth", requested), nil
}

func (x *resolution) entraCredential(scope string) (auth.Credential, error) {
	owner, ok := x.o().Providers.EntraOwner()
	if !ok {
		return auth.Credential{}, authError("Could not load the Azure Foundry Entra ID adapter. "+
			"Run icebath pm repair, then restart icebath. (the Entra ID provider is not registered)", "", "")
	}
	cred, err := owner.EntraCredential(x.ctx, firstOf(strings.TrimSpace(scope), defaultEntraScope))
	if err != nil {
		return auth.Credential{}, &auth.Error{Message: err.Error(), Cause: err}
	}
	return cred, nil
}

func (x *resolution) azureFoundryAPIKey(explicit string) (auth.Credential, error) {
	if explicit != "" {
		return staticKey(explicit, ""), nil
	}
	key, ok, err := x.o().Config.Env().Get(x.ctx, "AZURE_FOUNDRY_API_KEY")
	if err != nil || !ok {
		key = ""
	}
	if key == "" {
		if key, err = x.secretTrim("AZURE_FOUNDRY_API_KEY"); err != nil {
			return auth.Credential{}, err
		}
	}
	if key == "" {
		return auth.Credential{}, authError("Azure Foundry requires an API key. Set AZURE_FOUNDRY_API_KEY in "+
			"~/.icebath/.env or run 'icebath model' to configure. To use "+
			"keyless Microsoft Entra ID auth instead, set "+
			"model.auth_mode: entra_id in config.yaml (or pick "+
			"'Microsoft Entra ID' in 'icebath model').", "", "")
	}
	return staticKey(key, ""), nil
}

func (x *resolution) azureFoundry(requested string, mc *modelCfg, t Target, ek explicitCreds) (*Route, error) {
	var forwarded auth.TokenSource
	if ek.text == "" && t.Tokens != nil {
		forwarded = t.Tokens
	}
	explicitKey := ek.text
	explicitBase := trimURL(t.BaseURL)
	cfgBase, cfgMode, cfgAuth, scope := "", wire.ModeChat, "api_key", ""
	if cfgProvider(mc) == "azure-foundry" {
		cfgBase = x.configBaseURLFor(mc, "azure-foundry")
		cfgMode = firstMode(x.parseMode(mc.APIMode), wire.ModeChat)
		cfgAuth = firstOf(strings.ToLower(strings.TrimSpace(firstOf(mc.AuthMode, "api_key"))), "api_key")
		if mc.Entra != nil {
			scope = mc.Entra.Scope
		}
	}
	cfgMode = azureInferredMode(strings.TrimSpace(firstOf(t.Model, mc.Default)), cfgMode)
	envBase, err := x.secretTrim("AZURE_FOUNDRY_BASE_URL")
	if err != nil {
		return nil, err
	}
	base := firstOf(explicitBase, cfgBase, strings.TrimRight(envBase, "/"))
	if base == "" {
		return nil, authError("Azure Foundry requires a base URL. Set it via 'icebath model' or "+
			"the AZURE_FOUNDRY_BASE_URL environment variable.", "", "")
	}
	if cfgMode == wire.ModeAnthropic {
		base = trailingV1.ReplaceAllString(base, "")
	}
	if cfgAuth == "entra_id" {
		if explicitKey != "" {
			rt := x.route("azure-foundry", cfgMode, base, staticKey(explicitKey, ""), "explicit", requested)
			rt.AuthMode = "api_key"
			return rt, nil
		}
		var cred auth.Credential
		if forwarded != nil {
			cred = tokenKey(forwarded, "")
		} else if cred, err = x.entraCredential(scope); err != nil {
			return nil, err
		}
		rt := x.route("azure-foundry", cfgMode, base, cred, "entra_id", requested)
		rt.AuthMode, rt.EntraScope = "entra_id", strings.TrimSpace(scope)
		return rt, nil
	}
	key, err := x.azureFoundryAPIKey(explicitKey)
	if err != nil {
		return nil, err
	}
	source := "config"
	if explicitKey != "" || t.BaseURL != "" {
		source = "explicit"
	}
	rt := x.route("azure-foundry", cfgMode, base, key, source, requested)
	rt.AuthMode = "api_key"
	return rt, nil
}

func (x *resolution) openRouterRuntime(requested string, ek explicitCreds, explicitBase string) (*Route, error) {
	mc := x.model()
	cfgBase, cfgProv := mc.BaseURL, cfgProvider(mc)
	cfgKey := firstOf(strings.TrimSpace(mc.APIKey), strings.TrimSpace(mc.API))
	norm := strings.ToLower(strings.TrimSpace(requested))
	if norm != "" && norm != "custom" && x.resolvesToCustom(norm) {
		norm = "custom"
	}
	envORBase, err := x.secretTrim("OPENROUTER_BASE_URL")
	if err != nil {
		return nil, err
	}
	envCustomBase, err := x.secretTrim("CUSTOM_BASE_URL")
	if err != nil {
		return nil, err
	}
	useCfg := strings.TrimSpace(cfgBase) != "" && explicitBase == "" &&
		((norm == "auto" && (cfgProv == "" || cfgProv == "auto")) ||
			(norm == "custom" && x.trustworthyForBareCustom(cfgBase, cfgProv)) ||
			(norm == "openrouter" && cfgProv == "openrouter"))
	cfgChoice := ""
	if useCfg {
		cfgChoice = strings.TrimSpace(cfgBase)
	}
	base := strings.TrimRight(firstOf(strings.TrimSpace(explicitBase), envCustomBase, cfgChoice, envORBase, openRouterBaseURL), "/")
	isORURL := providers.HostMatches(base, "openrouter.ai")
	isORContext := isORURL || (norm == "openrouter" &&
		((useCfg && cfgProv == "openrouter" && base == trimURL(cfgBase)) ||
			(envORBase != "" && base == strings.TrimRight(envORBase, "/"))))
	declared, entryKey := false, noKey("")
	if norm == "custom" {
		if declared, entryKey, err = x.owningEntryCredential(base); err != nil {
			return nil, err
		}
	}
	candidates := []auth.Credential{ek.key}
	if isORContext {
		env := x.o().Config.Env()
		openAIKey, _, err := env.GetPreferFile(x.ctx, "OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		openAIBase, err := x.secretTrim("OPENAI_BASE_URL")
		if err != nil {
			return nil, err
		}
		var ok bool
		if providers.URLHostname(openAIBase) != "" {
			o := baseURLOrigin(openAIBase)
			ok = o.host != "" && o == baseURLOrigin(base)
		} else {
			ok = !isORURL || auth.LooksLikeOpenRouterKey(openAIKey)
		}
		if !declared {
			orKey, _, err := env.GetPreferFile(x.ctx, "OPENROUTER_API_KEY")
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, staticKey(orKey, ""))
			if ok {
				candidates = append(candidates, staticKey(openAIKey, ""))
			}
		}
	} else {
		if useCfg {
			keyEnv, err := x.modelKeyEnvFor(mc, base)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, staticKey(cfgKey, ""), staticKey(keyEnv, ""))
		}
		if !declared {
			gated, err := x.hostGatedCandidates(base, true)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, gated...)
		}
	}
	key := firstUsable(candidates)
	if !keyPresent(key) {
		key = entryKey
	}
	source := "env/config"
	if ek.raw || explicitBase != "" {
		source = "explicit"
	}
	cfgMode := x.parseMode(mc.APIMode)
	if norm != "custom" {
		return x.route("openrouter", firstMode(cfgMode, detectModeForURL(base), wire.ModeChat), base, key, source, ""), nil
	}
	if base != "" && !usableKey(ek.key) {
		if pooled, err := x.customPool(base, "custom", cfgMode, ""); err != nil || pooled != nil {
			return pooled, err
		}
	}
	if !keyPresent(key) && !isORURL {
		key = staticKey(noKeyPlaceholder, "")
	}
	return x.route("custom", x.plainCustomMode(mc, base), base, key, source, ""), nil
}

type origin struct {
	scheme string
	host   string
	port   int
}

func baseURLOrigin(raw string) origin {
	host := providers.URLHostname(raw)
	if host == "" {
		return origin{}
	}
	trimmed := strings.TrimSpace(raw)
	scheme := ""
	if i := strings.Index(trimmed, "://"); i >= 0 {
		scheme = strings.ToLower(trimmed[:i])
		trimmed = trimmed[i+3:]
	} else {
		trimmed = strings.TrimPrefix(trimmed, "//")
	}
	authority := trimmed
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	authority = authority[strings.LastIndex(authority, "@")+1:]
	portText := ""
	if strings.HasPrefix(authority, "[") {
		if i := strings.Index(authority, "]"); i >= 0 {
			portText = strings.TrimPrefix(authority[i+1:], ":")
		}
	} else if i := strings.LastIndex(authority, ":"); i >= 0 {
		portText = authority[i+1:]
	}
	o := origin{scheme: scheme, host: host}
	if portText == "" {
		switch scheme {
		case "https":
			o.port = 443
		case "http":
			o.port = 80
		}
		return o
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return origin{}
	}
	o.port = port
	return o
}
