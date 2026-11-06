package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Effective provider an auto routed client carries across cache reuse
type auxEffectiveStamp struct {
	v    atomic.Value
	dest atomic.Value
}

// Route identity a configured fallback entry attached to the client, false when none was attached
func (s *auxEffectiveStamp) auxFallbackDest() (auxFallbackDestination, bool) {
	d, ok := s.dest.Load().(auxFallbackDestination)
	return d, ok
}

func (s *auxEffectiveStamp) auxSetFallbackDest(d auxFallbackDestination) { s.dest.Store(d) }

type auxFallbackDestHolder interface {
	auxFallbackDest() (auxFallbackDestination, bool)
	auxSetFallbackDest(d auxFallbackDestination)
}

func (s *auxEffectiveStamp) auxEffectiveProvider() string {
	p, _ := s.v.Load().(string)
	return p
}

func (s *auxEffectiveStamp) auxSetEffectiveProvider(p string) { s.v.Store(p) }

type auxEffectiveProviderSetter interface{ auxSetEffectiveProvider(p string) }

// Stamps the provider on a client that can retain it, false for a client type holding no stamp
func auxStampEffectiveProvider(client AuxClient, provider string) bool {
	if s, ok := client.(auxEffectiveProviderSetter); ok {
		s.auxSetEffectiveProvider(provider)
		return true
	}
	return false
}

// Set once at startup so collected async clients never close their transport on a dead loop
var auxAsyncHTTPXDelNeutered atomic.Bool

// NeuterAsyncHTTPXDel makes the collection time transport close of every async auxiliary client a no op
func NeuterAsyncHTTPXDel() { auxAsyncHTTPXDelNeutered.Store(true) }

// Transport of an async client with its closed state, the httpx client wrapper of AsyncOpenAI
type auxAsyncTransport struct {
	closed atomic.Bool
	http   *http.Client
	owns   bool
}

// Collection time close of an async transport unless neutered or already closed
func auxAsyncTransportCollected(t *auxAsyncTransport) {
	if auxAsyncHTTPXDelNeutered.Load() || !t.closed.CompareAndSwap(false, true) {
		return
	}
	if t.owns {
		closeHTTPClient(t.http)
	}
}

// Async OpenAI SDK client whose transport closes when awaited or when the client is collected
type auxAsyncOpenAIClient struct {
	*auxOpenAIClient
	transport *auxAsyncTransport
}

// Async SDK client registering the collection time close of its transport
func newAuxAsyncOpenAIClient(apiKey any, baseURL string, opts auxOpenAIOptions) *auxAsyncOpenAIClient {
	inner := newAuxOpenAIClient(apiKey, baseURL, opts)
	t := &auxAsyncTransport{http: inner.httpClient, owns: inner.ownsHTTP}
	inner.ownsHTTP = false
	c := &auxAsyncOpenAIClient{auxOpenAIClient: inner, transport: t}
	runtime.AddCleanup(c, auxAsyncTransportCollected, t)
	return c
}

// Awaitable close of the transport, the AsyncOpenAI close coroutine
func (c *auxAsyncOpenAIClient) CloseAwaitable() func() error {
	return func() error {
		if c.transport.closed.CompareAndSwap(false, true) && c.transport.owns {
			closeHTTPClient(c.transport.http)
		}
		return nil
	}
}

// Marks the transport closed without closing it so collection never schedules the close
func (c *auxAsyncOpenAIClient) ForceCloseTransport() { c.transport.closed.Store(true) }

// Closes the transport in place
func (c *auxAsyncOpenAIClient) Close() error { return c.CloseAwaitable()() }

// Whether the transport is closed
func (c *auxAsyncOpenAIClient) IsClosed() bool { return c.transport.closed.Load() }

// Async counterpart of a sync client keeping its Codex, Anthropic, Bedrock and Gemini routing and its credential
func auxToAsyncClient(ctx context.Context, syncClient AuxClient, model string, isVision bool) (AuxClient, string) {
	switch c := syncClient.(type) {
	case *auxProbeClient:
		return c, model
	case *CodexAuxiliaryClient:
		return NewAsyncCodexAuxiliaryClient(c), model
	case *AnthropicAuxiliaryClient:
		return NewAsyncAnthropicAuxiliaryClient(c), model
	case *BedrockAuxiliaryClient:
		return NewAsyncBedrockAuxiliaryClient(c), model
	case *auxGeminiNativeClient:
		return newAuxAsyncGeminiNativeClient(c), model
	}
	if auxClientDeclares(syncClient, "ICEBATH_SKIP_ASYNC_WRAP") {
		return syncClient, model
	}
	syncBase := syncClient.BaseURL()
	var headers map[string]string
	switch {
	case HostMatches(syncBase, "openrouter.ai"):
		or, err := BuildORHeaders(ctx, nil)
		if err != nil {
			panic(err)
		}
		headers = auxApplyUserDefaultHeaders(ctx, or)
	case isOfficialCodexBaseURL(syncBase):
		headers = auxApplyUserDefaultHeaders(ctx, codexCloudflareHeaders(syncClient.APIKey(), syncBase))
	default:
		headers = auxEndpointDefaultHeaders(ctx, syncBase, inferProviderFromBaseURL(syncBase), isVision, true)
	}
	merged := map[string]string{}
	for k, v := range headers {
		merged[k] = v
	}
	for k, v := range auxConfiguredDefaultHeaders(syncClient) {
		merged[k] = v
	}
	var opts auxOpenAIOptions
	if len(merged) > 0 {
		opts.DefaultHeaders = merged
	}
	opts.DefaultHeaders = applyRequiredCodexHeaders(opts.DefaultHeaders, syncClient.APIKey(), syncBase)
	opts.HTTPClient = auxOpenAIHTTPClient(ctx, syncBase, true)
	noRetries := 0
	opts.MaxRetries = &noRetries
	return newAuxAsyncOpenAIClient(auxAsyncAPIKey(syncClient), syncBase, opts), model
}

// Seam for the named custom provider entry lookup tests patch
var auxGetNamedCustomProviderFn = func(ctx context.Context, name string) map[string]any {
	return modelsResolver(ctx).getNamedCustomProvider(name, nil, true)
}

// Named custom credential from inline api_key, key_env, key_cmd, the credential pool, else no-key-required
func auxNamedCustomAPIKey(ctx context.Context, entry map[string]any, provider, customBase string) (any, error) {
	var key any = strings.TrimSpace(cfgString(entry["api_key"]))
	keyEnv := strings.TrimSpace(auxFirstNonEmpty(cfgString(entry["key_env"]), cfgString(entry["api_key_env"])))
	if key == "" && keyEnv != "" {
		scoped, err := scopedKeyEnv(ctx, keyEnv)
		if err != nil {
			return nil, err
		}
		key = scoped
	}
	if keyCmd := strings.TrimSpace(auxPyStrOr(entry["key_cmd"], "")); keyCmd != "" {
		label := auxFirstNonEmpty(cfgString(entry["name"]), provider)
		if src := BuildCommandTokenSource(keyCmd, label, home.Dir(ctx), nil); src != nil {
			mintCtx := context.WithoutCancel(ctx)
			key = func() (string, error) { return src.Token(mintCtx) }
		}
	}
	if key == "" {
		_ = auxRecoverRaise(func() {
			cfg, err := auxLoadConfigReadonlyFn(ctx)
			if err != nil {
				return
			}
			poolName := auxFirstNonEmpty(cfgString(entry["provider_key"]), cfgString(entry["name"]), provider)
			for _, poolKey := range customProviderPoolKeyCandidates(customBase, poolName, cfg) {
				var pool *Pool
				if auxRecoverRaise(func() { pool = LoadPool(modelsPoolEnv(ctx), poolKey) }) != nil {
					continue
				}
				if pool = EntryScopedPool(pool, poolKey, entry); pool == nil || !pool.HasCredentials() {
					continue
				}
				selected, ok := pool.Select("")
				if !ok {
					continue
				}
				if k := auxPoolRuntimeAPIKey(&selected); k != "" {
					key = k
					break
				}
			}
		})
	}
	if key == "" {
		return "no-key-required", nil
	}
	return key, nil
}

// Bedrock client by model family, Anthropic SDK, Mantle Responses or Converse, nil without AWS credentials
func auxBuildBedrockClient(ctx context.Context, provider, model string, rawCodex bool) (AuxClient, string) {
	if !hasAWSCredentials(true) {
		slog.Debug("resolve_provider_client: bedrock requested but no AWS credentials found")
		return nil, ""
	}
	region := resolveBedrockRuntimeRegionCtx(ctx)
	const defaultModel = "anthropic.claude-haiku-4-5-20251001-v1:0"
	finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, defaultModel), provider)
	if finalModel == "" {
		finalModel = defaultModel
	}
	if bedrockOpenAIUsesMantle(finalModel) {
		key := resolveBedrockBearerToken()
		if key == "" {
			key = "aws-sdk"
		}
		route, err := ConfigureBedrockOpenAIRoute(ctx, Route{BaseURL: bedrockOpenAIBaseURL(region), APIKey: key}, 0)
		if err != nil {
			panic(err)
		}
		var opts auxOpenAIOptions
		if route.Transport != nil {
			opts.HTTPClient = &http.Client{Transport: route.Transport}
		}
		client := newAuxOpenAIClient(route.APIKey, route.BaseURL, opts)
		slog.Debug(fmt.Sprintf("resolve_provider_client: bedrock-openai (%s, %s)", finalModel, region))
		if rawCodex {
			return client, finalModel
		}
		return NewCodexAuxiliaryClient(client, finalModel), finalModel
	}
	baseURL := "https://bedrock-runtime." + region + ".amazonaws.com"
	if isAnthropicBedrockModel(finalModel) {
		realClient, err := buildAnthropicBedrockClient(ctx, region, 0)
		if err != nil {
			slog.Warn(fmt.Sprintf("resolve_provider_client: cannot create Bedrock client: %v", err))
			return nil, ""
		}
		slog.Debug(fmt.Sprintf("resolve_provider_client: bedrock anthropic (%s, %s)", finalModel, region))
		return newAnthropicAuxiliaryClientFrom(realClient, finalModel, "aws-sdk", baseURL, false), finalModel
	}
	slog.Debug(fmt.Sprintf("resolve_provider_client: bedrock converse (%s, %s)", finalModel, region))
	return NewBedrockAuxiliaryClient(region, finalModel), finalModel
}

// Vertex Gemini over the OpenAI compatible endpoint with a minted bearer, nil without GCP credentials
func auxBuildVertexClient(ctx context.Context, provider, model string) (AuxClient, string) {
	h := home.Dir(ctx)
	env := vertexEnv{cfg: LoadConfigSafe(h), secrets: config.NewSecrets(h)}
	if !hasVertexCredentials(ctx, env) {
		slog.Debug("resolve_provider_client: vertex requested but no GCP credentials found")
		return nil, ""
	}
	token, baseURL := vertexGetConfig(ctx, env, "", "")
	if token == "" || baseURL == "" {
		slog.Warn("resolve_provider_client: vertex requested but could not mint token / resolve project")
		return nil, ""
	}
	finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, "google/gemini-3-flash-preview"), provider)
	slog.Debug(fmt.Sprintf("resolve_provider_client: vertex (%s)", finalModel))
	return newAuxOpenAIClient(token, baseURL, auxOpenAIOptions{}), finalModel
}

// Normalized resolve_provider_client arguments the branch helpers share
type auxResolveRequest struct {
	Provider         string
	OriginalProvider string
	Model            string
	AsyncMode        bool
	RawCodex         bool
	ExplicitBaseURL  string
	ExplicitAPIKey   any
	APIMode          string
	MainRuntime      map[string]any
	IsVision         bool
	Task             string
}

// Client and model a branch resolved, nil client and empty model for none
type auxResolveResult struct {
	Client AuxClient
	Model  string
}

// Whether the provider or the same origin main runtime is an Actual route
func auxIsActualAuxiliaryRoute(ctx context.Context, req auxResolveRequest, baseURL string) bool {
	if isActualRouteCtx(ctx, req.Provider, baseURL) {
		return true
	}
	rt := auxNormalizeMainRuntime(ctx, req.MainRuntime)
	rtProvider, _ := rt["provider"].(string)
	rtBase, _ := rt["base_url"].(string)
	return baseURL != "" && isActualRouteCtx(ctx, rtProvider, rtBase) &&
		NormalizeRouteBaseURL(NormalizeActualBaseURL(baseURL)) == NormalizeRouteBaseURL(NormalizeActualBaseURL(rtBase))
}

// SDK client with no Responses surface, raising AttributeError when the Codex adapter reaches for it
type auxNoResponsesClient struct{ AuxClient }

func (c auxNoResponsesClient) CreateResponse(context.Context, map[string]any) (any, error) {
	return nil, &auxAttributeError{typeName: auxTypeName(c.AuxClient), attr: "responses"}
}

// Python class name of a client value
func auxTypeName(v any) string {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return "NoneType"
	}
	return t.Name()
}

// Client as the Responses capable SDK client the Codex adapter drives
func auxAsResponsesClient(client AuxClient) auxResponsesClient {
	if rc, ok := client.(auxResponsesClient); ok {
		return rc
	}
	return auxNoResponsesClient{client}
}

// Plain client wrapped in the Codex or Anthropic adapter its route needs, Actual routes unwrapped and stamped
func auxWrapTransport(ctx context.Context, req auxResolveRequest, client AuxClient, finalModel, baseURL string, apiKey any) (AuxClient, error) {
	if auxIsActualAuxiliaryRoute(ctx, req, baseURL) {
		target := client
		if cc, ok := client.(*CodexAuxiliaryClient); ok {
			target = cc.realClient
		}
		auxStampEffectiveProvider(target, "actual")
		return target, nil
	}
	if mode, base := OpencodeTransport(ctx, req.Provider, finalModel, baseURL); mode != "" {
		req.APIMode, baseURL = string(mode), base
	}
	_, isCodex := client.(*CodexAuxiliaryClient)
	needsCodex := !(isCodex || req.RawCodex) && (req.APIMode == "codex_responses" ||
		(req.APIMode == "" && baseURLHostname(baseURL) == "api.openai.com" && strings.Contains(strings.ToLower(finalModel), "codex")))
	if needsCodex {
		shown := baseURL
		if len(shown) > 60 {
			shown = shown[:60]
		}
		slog.Debug(fmt.Sprintf("resolve_provider_client: wrapping client in CodexAuxiliaryClient (api_mode=%s, model=%s, base_url=%s)",
			auxFirstNonEmpty(req.APIMode, "auto-detected"), finalModel, shown))
		return NewCodexAuxiliaryClient(auxAsResponsesClient(client), finalModel), nil
	}
	apiMode := req.APIMode
	if apiMode == "" {
		apiMode = auxProfileDeclaredMessagesWire(ctx, req.Provider)
	}
	return auxMaybeWrapAnthropic(ctx, client, finalModel, apiKey, baseURL, apiMode)
}

// anthropic_messages when the registered profile declares that wire, else empty
func auxProfileDeclaredMessagesWire(ctx context.Context, provider string) string {
	profile := getProviderProfile(ctx, strings.ToLower(strings.TrimSpace(provider)))
	if profile != nil && profile.Mode == ModeAnthropicMessages {
		return "anthropic_messages"
	}
	return ""
}

// Client and model, converted to the async wrapper when the request is async
func auxRouteClient(ctx context.Context, req auxResolveRequest, client AuxClient, model string) auxResolveResult {
	if req.AsyncMode {
		c, m := auxToAsyncClient(ctx, client, model, req.IsVision)
		return auxResolveResult{Client: c, Model: m}
	}
	return auxResolveResult{Client: client, Model: model}
}

// Routes the client on the request model or the default, warning and resolving nothing for a nil client
func auxRouteOrWarn(ctx context.Context, req auxResolveRequest, client AuxClient, def, unavailable string) auxResolveResult {
	if client == nil {
		slog.Warn(unavailable)
		return auxResolveResult{}
	}
	return auxRouteClient(ctx, req, client, auxNormalizeResolvedModel(auxFirstNonEmpty(req.Model, def), req.Provider))
}

// Full auto detection chain with the selected provider stamped on the client
func auxResolveAutoBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, resolved, effective, err := auxResolveAutoRoute(ctx, req.MainRuntime, req.Task)
	if err != nil || client == nil {
		return auxResolveResult{}, err
	}
	model := req.Model
	if model != "" && strings.Contains(model, "/") && resolved != "" && !strings.Contains(resolved, "/") {
		slog.Debug(fmt.Sprintf("Dropping OpenRouter-format model %s for non-OpenRouter auxiliary provider (using %s instead)", pyrepr.Repr(model), pyrepr.Repr(resolved)))
		model = ""
	}
	routed := auxRouteClient(ctx, req, client, auxFirstNonEmpty(model, resolved))
	if routed.Client != nil && effective != "" && !auxStampEffectiveProvider(routed.Client, effective) {
		slog.Debug(fmt.Sprintf("Auxiliary client %s cannot retain effective provider %s", auxTypeName(routed.Client), effective))
	}
	return routed, nil
}

// OpenRouter with the explicit key, model and base URL
func auxResolveOpenRouterBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, def, err := auxTryOpenrouter(ctx, req.ExplicitAPIKey, req.Model, req.ExplicitBaseURL)
	if err != nil {
		return auxResolveResult{}, err
	}
	if client == nil {
		why, err := auxDescribeOpenrouterUnavailable(ctx, req.Model)
		if err != nil {
			return auxResolveResult{}, err
		}
		slog.Warn("resolve_provider_client: openrouter requested but " + why)
		return auxResolveResult{}, nil
	}
	return auxRouteClient(ctx, req, client, auxNormalizeResolvedModel(auxFirstNonEmpty(req.Model, def), req.Provider)), nil
}

// Nous Portal on the wire its catalog model rides, Messages for anthropic models on the native route
func auxResolveNousBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	model := req.Model
	vision := req.IsVision || strings.ToLower(strings.TrimSpace(model)) == "mimo-v2-omni"
	for _, v := range auxProviderVisionModels {
		vision = vision || (model != "" && model == v)
	}
	client, def, err := auxTryNous(ctx, vision)
	if err != nil {
		return auxResolveResult{}, err
	}
	if client == nil {
		slog.Warn("resolve_provider_client: nous requested but Nous Portal not configured (run: icebath auth)")
		return auxResolveResult{}, nil
	}
	finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, def), req.Provider)
	cfg, _ := auxLoadConfigReadonlyFn(ctx)
	wire := NousMode(finalModel, newResolver(ctx, cfg, nil, "", nil).nousAnthropicWire())
	wrapped, err := auxMaybeWrapAnthropic(ctx, client, finalModel, client.APIKey(), client.BaseURL(), string(wire))
	if err != nil {
		return auxResolveResult{}, err
	}
	return auxRouteClient(ctx, req, wrapped, finalModel), nil
}

// OpenAI Codex over the Responses API, the raw SDK client for responses stream callers
func auxResolveOpenAICodexBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	model := req.Model
	if model == "" {
		slog.Warn("resolve_provider_client: openai-codex requested without a model; pass model explicitly (e.g. model.model in config.yaml or auxiliary.<task>.model for per-task aux routing).")
		return auxResolveResult{}, nil
	}
	const noToken = "resolve_provider_client: openai-codex requested but no Codex OAuth token found (run: icebath model)"
	if req.RawCodex {
		token, baseURL, err := auxResolveCodexCredentialAndBase(ctx)
		if err != nil {
			return auxResolveResult{}, err
		}
		if token == "" {
			slog.Warn(noToken)
			return auxResolveResult{}, nil
		}
		raw := auxCreateOpenAIClient(ctx, token, baseURL, auxOpenAIOptions{DefaultHeaders: codexCloudflareHeaders(token, baseURL)})
		return auxResolveResult{Client: raw, Model: auxNormalizeResolvedModel(model, req.Provider)}, nil
	}
	client, def, err := auxBuildCodexClient(ctx, model)
	if err != nil {
		return auxResolveResult{}, err
	}
	return auxRouteOrWarn(ctx, req, client, def, noToken), nil
}

// xAI Grok OAuth over the Responses API
func auxResolveXAIOAuthBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, def, err := auxBuildXaiOAuthAuxClient(ctx, req.Model)
	if err != nil {
		return auxResolveResult{}, err
	}
	return auxRouteOrWarn(ctx, req, client, def,
		"resolve_provider_client: xai-oauth requested but no xAI OAuth token found (run: icebath model -> xAI Grok OAuth — SuperGrok / Premium+)"), nil
}

// Explicit custom endpoint, the main runtime's endpoint, then the custom and API key provider chain
func auxResolveCustomBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	provider, model, mainRuntime := req.Provider, req.Model, req.MainRuntime
	var customBase, wrapBase string
	var customKey any = ""
	_, localAlias := auxLocalServerAliases[req.OriginalProvider]
	if req.ExplicitBaseURL != "" {
		customBase = strings.TrimSpace(auxToOpenAIBaseURL(req.ExplicitBaseURL))
		if localAlias {
			path := ""
			if u, err := url.Parse(customBase); err == nil {
				path = u.Path
			}
			if strings.Trim(path, "/") == "" {
				customBase = strings.TrimRight(customBase, "/") + "/v1"
			}
		}
		if req.APIMode == "anthropic_messages" {
			wrapBase = strings.TrimRight(strings.TrimSpace(req.ExplicitBaseURL), "/")
		}
		if localAlias {
			customKey = auxNormalizeAPIKey(req.ExplicitAPIKey)
			if !auxTruthyKey(customKey) {
				customKey = "no-key-required"
			}
		} else {
			customKey = auxNormalizeAPIKey(req.ExplicitAPIKey)
			if !auxTruthyKey(customKey) {
				scoped, err := scopedKeyEnv(ctx, "OPENAI_API_KEY")
				if err != nil {
					return auxResolveResult{}, err
				}
				customKey = scoped
			}
			if !auxTruthyKey(customKey) {
				mainKey, err := auxReadMainAPIKeyIfSameOrigin(ctx, customBase)
				if err != nil {
					return auxResolveResult{}, err
				}
				customKey = mainKey
			}
			if !auxTruthyKey(customKey) {
				customKey = "no-key-required"
			}
		}
		if customBase == "" {
			slog.Warn("resolve_provider_client: explicit custom endpoint requested but base_url is empty")
			return auxResolveResult{}, nil
		}
	} else if len(mainRuntime) > 0 {
		mainBase := strings.TrimRight(strings.TrimSpace(auxPyStrOr(mainRuntime["base_url"], "")), "/")
		mainKey := auxNormalizeAPIKey(mainRuntime["api_key"])
		if mainBase != "" && auxTruthyKey(mainKey) {
			customBase, customKey = mainBase, mainKey
		}
	}
	if customBase != "" && auxTruthyKey(customKey) {
		if auxIsActualAuxiliaryRoute(ctx, req, customBase) {
			customBase = NormalizeActualBaseURL(customBase)
		}
		var mainModel string
		if mainRuntime != nil {
			mainModel = cfgString(mainRuntime["model"])
		}
		finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, mainModel, "gpt-4o-mini"), provider)
		cleanBase, query, err := auxExtractURLQueryParams(customBase)
		if err != nil {
			return auxResolveResult{}, err
		}
		var opts auxOpenAIOptions
		if len(query) > 0 {
			opts.DefaultQuery = query
		}
		cfg, _ := auxLoadConfigReadonlyFn(ctx)
		headers := map[string]string{}
		for k, v := range auxEndpointDefaultHeaders(ctx, customBase, provider, req.IsVision, false) {
			headers[k] = v
		}
		for k, v := range CustomProviderExtraHeaders(customBase, nil, cfg) {
			headers[k] = v
		}
		if len(headers) > 0 {
			opts.DefaultHeaders = headers
		}
		client := auxCreateOpenAIClient(ctx, customKey, cleanBase, opts)
		wrapped, err := auxWrapTransport(ctx, req, client, finalModel, auxFirstNonEmpty(wrapBase, customBase), customKey)
		if err != nil {
			return auxResolveResult{}, err
		}
		return auxRouteClient(ctx, req, wrapped, finalModel), nil
	}
	for _, try := range []func(context.Context) (AuxClient, string, error){auxTryCustomEndpoint, auxResolveAPIKeyProvider} {
		client, def, err := try(ctx)
		if err != nil {
			return auxResolveResult{}, err
		}
		if client == nil {
			continue
		}
		finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, def), provider)
		wrapped, err := auxWrapTransport(ctx, req, client, finalModel, client.BaseURL(), client.APIKey())
		if err != nil {
			return auxResolveResult{}, err
		}
		return auxRouteClient(ctx, req, wrapped, finalModel), nil
	}
	slog.Warn("resolve_provider_client: custom/main requested but no endpoint credentials found")
	return auxResolveResult{}, nil
}

// Plain OpenAI client on the v1 form of a named entry's base URL carrying the entry's own headers
func auxNamedCustomOpenAIWireClient(ctx context.Context, customBase string, customKey any, extraHeaders map[string]string) (AuxClient, error) {
	cleanBase, query, err := auxExtractURLQueryParams(auxToOpenAIBaseURL(customBase))
	if err != nil {
		return nil, err
	}
	var opts auxOpenAIOptions
	if len(query) > 0 {
		opts.DefaultQuery = query
	}
	headers := auxApplyUserDefaultHeaders(ctx, nil)
	if len(extraHeaders) > 0 {
		merged := map[string]string{}
		for k, v := range headers {
			merged[k] = v
		}
		for k, v := range extraHeaders {
			merged[k] = v
		}
		headers = merged
	}
	if len(headers) > 0 {
		opts.DefaultHeaders = headers
	}
	return auxCreateOpenAIClient(ctx, customKey, cleanBase, opts), nil
}

// Named custom provider entry composed under the caller's explicit endpoint, found false when no entry matches
func auxResolveNamedCustomBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, bool, error) {
	provider := req.Provider
	var entry map[string]any
	if req.OriginalProvider != "" && req.OriginalProvider != provider {
		entry = auxGetNamedCustomProviderFn(ctx, req.OriginalProvider)
	}
	if entry == nil {
		entry = auxGetNamedCustomProviderFn(ctx, provider)
	}
	if len(entry) == 0 {
		return auxResolveResult{}, false, nil
	}
	customBase := strings.TrimSpace(auxFirstNonEmpty(req.ExplicitBaseURL, cfgString(entry["base_url"])))
	customKey := auxNormalizeAPIKey(req.ExplicitAPIKey)
	if !auxTruthyKey(customKey) {
		k, err := auxNamedCustomAPIKey(ctx, entry, provider, customBase)
		if err != nil {
			return auxResolveResult{}, true, err
		}
		customKey = k
	}
	if customKey == "no-key-required" {
		slog.Warn(fmt.Sprintf("resolve_provider_client: named custom provider %s has no resolvable api_key — request will be sent with placeholder no-key-required and will 401 on auth-required endpoints",
			pyrepr.Repr(auxFirstNonEmpty(cfgString(entry["name"]), provider))))
	}
	entryAPIMode := strings.TrimSpace(auxFirstNonEmpty(req.APIMode, cfgString(entry["api_mode"])))
	if auxIsActualAuxiliaryRoute(ctx, req, customBase) {
		customBase = NormalizeActualBaseURL(customBase)
		entryAPIMode = "chat_completions"
	}
	if customBase == "" {
		slog.Warn(fmt.Sprintf("resolve_provider_client: named custom provider %s has no base_url", pyrepr.Repr(provider)))
		return auxResolveResult{}, true, nil
	}
	entryHeaders := NormalizeExtraHeaders(entry["extra_headers"])
	var mainModel string
	if req.MainRuntime != nil {
		mainModel = cfgString(req.MainRuntime["model"])
	}
	model := auxFirstNonEmpty(req.Model, cfgString(entry["model"]), mainModel)
	if model == "" {
		model = auxReadMainModelForAux(ctx)
	}
	finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(model, "gpt-4o-mini"), provider)
	if mode, base := OpencodeTransport(ctx, provider, finalModel, customBase); mode != "" {
		entryAPIMode, customBase = string(mode), base
	}
	slog.Debug(fmt.Sprintf("resolve_provider_client: named custom provider %s (%s, api_mode=%s)", pyrepr.Repr(provider), finalModel, auxFirstNonEmpty(entryAPIMode, "chat_completions")))
	if entryAPIMode == "anthropic_messages" {
		realClient, err := buildAnthropicClient(ctx, customKey, customBase, 0, false)
		if err != nil {
			return auxResolveResult{}, true, err
		}
		if len(entryHeaders) > 0 {
			realClient = realClient.withDefaultHeaders(entryHeaders)
		}
		keyStr, _ := customKey.(string)
		client := newAnthropicAuxiliaryClientFrom(realClient, finalModel, customKey, customBase, anthropicRouteIsOAuth(customBase, keyStr, ""))
		return auxRouteClient(ctx, req, client, finalModel), true, nil
	}
	client, err := auxNamedCustomOpenAIWireClient(ctx, customBase, customKey, entryHeaders)
	if err != nil {
		return auxResolveResult{}, true, err
	}
	if entryAPIMode == "codex_responses" {
		client = NewCodexAuxiliaryClient(auxAsResponsesClient(client), finalModel)
	} else if client, err = auxWrapTransport(ctx, req, client, finalModel, customBase, customKey); err != nil {
		return auxResolveResult{}, true, err
	}
	return auxRouteClient(ctx, req, client, finalModel), true, nil
}

// Azure Foundry through the runtime resolver honouring Entra bearers and config base URLs
func auxResolveAzureFoundryBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	client, def, err := auxTryAzureFoundry(ctx, req.Model, req.ExplicitAPIKey, req.ExplicitBaseURL, req.APIMode)
	if err != nil {
		return auxResolveResult{}, err
	}
	return auxRouteOrWarn(ctx, req, client, def,
		"resolve_provider_client: azure-foundry requested but runtime resolution failed (run: icebath doctor for diagnostics)"), nil
}

// Registered profile's own client for an api_key route, nil when it supplies none or fails building one
func auxAPIKeyProfileSuppliedClient(ctx context.Context, provider string, apiKey any, baseURL string) AuxClient {
	var profile *Profile
	if auxRecoverRaise(func() { profile = getProviderProfile(ctx, provider) }) != nil || profile == nil {
		return nil
	}
	var client Client
	err := auxRecoverRaise(func() {
		var cerr error
		if client, cerr = profile.CreateClient(ctx, map[string]any{"api_key": apiKey, "base_url": baseURL}); cerr != nil {
			panic(cerr)
		}
	})
	if err != nil {
		slog.Warn(fmt.Sprintf("resolve_provider_client: provider profile %s failed to create an auxiliary client; falling back to the standard client path", pyrepr.Repr(provider)), "err", err)
		return nil
	}
	if client == nil {
		return nil
	}
	if aux, ok := client.(AuxClient); ok {
		return aux
	}
	return auxProviderClientAux(ctx, client)
}

// Registry api_key provider honouring explicit key and base URL overrides, Anthropic through its own resolver
func auxResolveAPIKeyBranch(ctx context.Context, req auxResolveRequest, pconfig *Profile) (auxResolveResult, error) {
	provider := req.Provider
	if provider == "anthropic" {
		client, def, err := auxTryAnthropic(ctx, req.ExplicitAPIKey, req.ExplicitBaseURL)
		if err != nil {
			return auxResolveResult{}, err
		}
		return auxRouteOrWarn(ctx, req, client, def, "resolve_provider_client: anthropic requested but no Anthropic credentials found"), nil
	}
	creds, err := auxResolveAPIKeyProviderCredentialsFn(ctx, provider)
	if err != nil {
		return auxResolveResult{}, err
	}
	var apiKey any = strings.TrimSpace(creds.APIKey)
	if explicit := auxNormalizeAPIKey(req.ExplicitAPIKey); auxTruthyKey(explicit) {
		apiKey = explicit
	}
	rawBaseURL := strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
	if rawBaseURL == "" {
		rawBaseURL = registryInferenceBaseURL(pconfig)
	}
	if req.ExplicitBaseURL != "" {
		rawBaseURL = strings.TrimRight(strings.TrimSpace(req.ExplicitBaseURL), "/")
	}
	if provider == "actual" {
		rawBaseURL = NormalizeActualBaseURL(rawBaseURL)
		if !auxTruthyKey(apiKey) && isActualLocalBaseURL(rawBaseURL) {
			apiKey = ActualLocalNoAuthPlaceholder
		}
	}
	if !auxTruthyKey(apiKey) {
		slog.Debug(fmt.Sprintf("resolve_provider_client: provider %s has no API key configured (tried: %s)", provider, strings.Join(pconfig.KeyEnv, ", ")))
		return auxResolveResult{}, nil
	}
	baseURL := auxToOpenAIBaseURL(rawBaseURL)
	if req.ExplicitBaseURL != "" && provider != "actual" {
		baseURL = auxToOpenAIBaseURL(strings.TrimRight(strings.TrimSpace(req.ExplicitBaseURL), "/"))
	}
	finalModel := auxNormalizeResolvedModel(auxFirstNonEmpty(req.Model, auxGetAuxModelForProvider(ctx, provider)), provider)
	if profileClient := auxAPIKeyProfileSuppliedClient(ctx, provider, apiKey, baseURL); profileClient != nil {
		slog.Debug(fmt.Sprintf("resolve_provider_client: %s native client from provider profile (%s)", provider, finalModel))
		return auxRouteClient(ctx, req, profileClient, finalModel), nil
	}
	keyStr, _ := apiKey.(string)
	if provider == "gemini" && isNativeGeminiBaseURL(baseURL) {
		client, err := newAuxGeminiNativeClient(keyStr, baseURL, nil)
		if err != nil {
			return auxResolveResult{}, err
		}
		slog.Debug(fmt.Sprintf("resolve_provider_client: %s (%s)", provider, finalModel))
		return auxRouteClient(ctx, req, client, finalModel), nil
	}
	var opts auxOpenAIOptions
	if headers := auxEndpointDefaultHeaders(ctx, baseURL, provider, req.IsVision, true); len(headers) > 0 {
		opts.DefaultHeaders = headers
	}
	client := auxCreateOpenAIClient(ctx, apiKey, baseURL, opts)
	if finalModel != "" && !req.RawCodex {
		if profile := getProviderProfile(ctx, provider); profile != nil {
			if mode, ok := ProfileModelAPIMode(ctx, *profile, finalModel); ok && mode == "codex_responses" {
				slog.Debug(fmt.Sprintf("resolve_provider_client: %s model %s needs Responses API — wrapping", provider, finalModel))
				client = NewCodexAuxiliaryClient(auxAsResponsesClient(client), finalModel)
			}
		}
	}
	wrapped, err := auxWrapTransport(ctx, req, client, finalModel, rawBaseURL, apiKey)
	if err != nil {
		return auxResolveResult{}, err
	}
	slog.Debug(fmt.Sprintf("resolve_provider_client: %s (%s)", provider, finalModel))
	return auxRouteClient(ctx, req, wrapped, finalModel), nil
}

// Registry external_process provider served through its registered profile's client
func auxResolveExternalProcessBranch(ctx context.Context, req auxResolveRequest, creds *Runtime) (auxResolveResult, error) {
	provider := req.Provider
	var mainModel string
	if req.MainRuntime != nil {
		mainModel = cfgString(req.MainRuntime["model"])
	}
	model := auxFirstNonEmpty(req.Model, mainModel)
	if model == "" {
		model = auxReadMainModelForAux(ctx)
	}
	finalModel := auxNormalizeResolvedModel(model, provider)
	var profile *Profile
	_ = auxRecoverRaise(func() { profile = getProviderProfile(ctx, provider) })
	if profile != nil {
		apiKey, baseURL := strings.TrimSpace(creds.APIKey), strings.TrimSpace(creds.BaseURL)
		if finalModel == "" {
			slog.Warn(fmt.Sprintf("resolve_provider_client: %s requested but no model was provided or configured", provider))
			return auxResolveResult{}, nil
		}
		if apiKey == "" || baseURL == "" {
			slog.Warn(fmt.Sprintf("resolve_provider_client: %s requested but external process credentials are incomplete", provider))
			return auxResolveResult{}, nil
		}
		var command any
		if c := strings.TrimSpace(creds.Command); c != "" {
			command = c
		}
		args := append([]string{}, creds.Args...)
		var client Client
		err := auxRecoverRaise(func() {
			var cerr error
			if client, cerr = profile.CreateClient(ctx, map[string]any{"api_key": apiKey, "base_url": baseURL, "command": command, "args": args}); cerr != nil {
				panic(cerr)
			}
		})
		if err != nil {
			slog.Warn(fmt.Sprintf("resolve_provider_client: profile %s failed to create an external-process client", pyrepr.Repr(provider)), "err", err)
			client = nil
		}
		if client != nil {
			aux, ok := client.(AuxClient)
			if !ok {
				aux = auxProviderClientAux(ctx, client)
			}
			slog.Debug(fmt.Sprintf("resolve_provider_client: %s (%s)", provider, finalModel))
			return auxRouteClient(ctx, req, aux, finalModel), nil
		}
	}
	auxLogOnceDebug(auxLoggedUnsupportedExtProcKeys, provider, "resolve_provider_client: external-process provider %s not directly supported", provider)
	return auxResolveResult{}, nil
}

// Registry provider dispatched on its auth type, unknown providers and auth types logged once
func auxResolveRegistryBranch(ctx context.Context, req auxResolveRequest) (auxResolveResult, error) {
	provider := req.Provider
	pconfig := authRegistryGet(ctx, provider)
	if pconfig == nil {
		auxLogOnceDebug(auxLoggedUnknownProviderKeys, provider, "resolve_provider_client: unknown provider %s", pyrepr.Repr(provider))
		return auxResolveResult{}, nil
	}
	authType := pconfig.AuthType
	switch authType {
	case AuthTypeAPIKey:
		return auxResolveAPIKeyBranch(ctx, req, pconfig)
	case AuthTypeExternalProcess:
		creds, err := modelsResolver(ctx).resolveExternalProcessRuntime(provider, provider)
		if err != nil {
			return auxResolveResult{}, err
		}
		return auxResolveExternalProcessBranch(ctx, req, creds)
	}
	if arm, ok := RegistryAuthTypeArms[authType]; ok && arm != nil {
		return arm(ctx, req)
	}
	auxLogOnceDebug(auxLoggedUnhandledAuthTypeKeys, authType+"\x00"+provider, "resolve_provider_client: unhandled auth_type %s for %s", authType, provider)
	return auxResolveResult{}, nil
}

// Explicit providers with a dedicated branch, everything else goes named custom, azure foundry, then the registry
var auxExplicitProviderBranches map[string]func(context.Context, auxResolveRequest) (auxResolveResult, error)

func init() {
	auxExplicitProviderBranches = map[string]func(context.Context, auxResolveRequest) (auxResolveResult, error){
		"auto":         auxResolveAutoBranch,
		"openrouter":   auxResolveOpenRouterBranch,
		"nous":         auxResolveNousBranch,
		"openai-codex": auxResolveOpenAICodexBranch,
		"xai-oauth":    auxResolveXAIOAuthBranch,
		"custom":       auxResolveCustomBranch,
	}
}

// Arguments of resolve_provider_client beyond provider and model
type ProviderClientOptions struct {
	AsyncMode       bool
	RawCodex        bool
	ExplicitBaseURL string
	ExplicitAPIKey  any
	APIMode         string
	MainRuntime     map[string]any
	IsVision        bool
	Task            string
}

// ResolveProviderClient builds the configured client and model for a provider, nil and empty when unavailable
func ResolveProviderClient(ctx context.Context, provider, model string, opts ProviderClientOptions) (AuxClient, string, error) {
	if err := auxValidateProxyEnvURLs(); err != nil {
		return nil, "", err
	}
	var res auxResolveResult
	var err error
	if raised := auxRecoverRaise(func() { res, err = auxResolveProviderClient(ctx, provider, model, opts) }); raised != nil {
		return nil, "", raised
	}
	if err != nil {
		return nil, "", err
	}
	return res.Client, res.Model, nil
}

// Body of the central router after proxy validation
func auxResolveProviderClient(ctx context.Context, provider, model string, opts ProviderClientOptions) (auxResolveResult, error) {
	originalProvider := strings.ToLower(strings.TrimSpace(provider))
	provider = auxNormalizeAuxProvider(ctx, provider)
	apiMode := strings.ToLower(CanonicalAPIMode(opts.APIMode))
	explicitBase, explicitKey := opts.ExplicitBaseURL, opts.ExplicitAPIKey
	if provider == "moa" {
		aggProvider, aggModel := auxResolveMoaAggregator(ctx, model)
		if aggProvider != "" && aggModel != "" {
			originalProvider = strings.ToLower(strings.TrimSpace(aggProvider))
			provider = auxNormalizeAuxProvider(ctx, aggProvider)
			model = aggModel
			if explicitBase != "" && strings.HasPrefix(strings.ToLower(explicitBase), "moa://") {
				explicitBase, explicitKey = "", nil
			}
		}
	}
	keyStr, _ := explicitKey.(string)
	localBase, localKey, local, err := auxBareLlamacppEndpoint(ctx, originalProvider, explicitBase, keyStr)
	if err != nil {
		return auxResolveResult{}, err
	}
	if local {
		if localBase == "" {
			slog.Warn(fmt.Sprintf("resolve_provider_client: %s requested but no local llama.cpp server is running", originalProvider))
			return auxResolveResult{}, nil
		}
		explicitBase, explicitKey = localBase, localKey
	}
	if model == "" && provider != "auto" && !(provider == "nous" && opts.IsVision) {
		model = auxGetAuxModelForProvider(ctx, provider)
		if model == "" {
			model = auxReadMainModelForAux(ctx)
		}
	}
	req := auxResolveRequest{Provider: provider, OriginalProvider: originalProvider, Model: model, AsyncMode: opts.AsyncMode,
		RawCodex: opts.RawCodex, ExplicitBaseURL: explicitBase, ExplicitAPIKey: explicitKey, APIMode: apiMode,
		MainRuntime: opts.MainRuntime, IsVision: opts.IsVision, Task: opts.Task}
	branch := auxExplicitProviderBranches[provider]
	_, aliasIsLocal := auxLocalServerAliases[strings.TrimPrefix(originalProvider, "custom:")]
	if branch == nil || aliasIsLocal {
		res, found, err := auxResolveNamedCustomBranch(ctx, req)
		if err != nil || found {
			return res, err
		}
	}
	if branch != nil {
		return branch(ctx, req)
	}
	if provider == "azure-foundry" {
		return auxResolveAzureFoundryBranch(ctx, req)
	}
	return auxResolveRegistryBranch(ctx, req)
}

// GetTextAuxiliaryClient is the client and model for a text auxiliary task under its auxiliary.<task> overrides
func GetTextAuxiliaryClient(ctx context.Context, task string, mainRuntime map[string]any) (AuxClient, string, error) {
	var taskArg any
	if task != "" {
		taskArg = task
	}
	route := auxResolveTaskProviderModel(ctx, taskArg, "", "", "", nil)
	return ResolveProviderClient(ctx, route.Provider, route.Model, ProviderClientOptions{
		ExplicitBaseURL: route.BaseURL, ExplicitAPIKey: route.APIKey, APIMode: route.APIMode, MainRuntime: mainRuntime,
	})
}
