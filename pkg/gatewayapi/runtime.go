package gatewayapi

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Validated model routes, invalid shapes dropped with a warning and never raised
func parseModelRoutes(raw any) *modelRouteTable {
	table := &modelRouteTable{routes: map[string]map[string]string{}}
	var keys []string
	var get func(string) any
	switch m := raw.(type) {
	case *pyjson.Object:
		keys = m.Keys
		get = func(k string) any { return m.GetOr(k, nil) }
	case map[string]any:
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		get = func(k string) any { return m[k] }
	default:
		if pyrepr.Truthy(raw) {
			apiLog().Warn(fmt.Sprintf("api_server model_routes ignored: expected a mapping, got %s", pyrepr.TypeName(raw)))
		}
		return table
	}
	for _, alias := range keys {
		aliasStr := strings.TrimSpace(alias)
		cfg := get(alias)
		var fields func(string) (any, bool)
		switch c := cfg.(type) {
		case *pyjson.Object:
			fields = c.Get
		case map[string]any:
			fields = func(k string) (any, bool) { v, ok := c[k]; return v, ok }
		}
		if aliasStr == "" || fields == nil {
			label := any(aliasStr)
			if aliasStr == "" {
				label = alias
			}
			apiLog().Warn(fmt.Sprintf("api_server model_routes: dropping invalid route entry %s", pyrepr.Repr(label)))
			continue
		}
		route := map[string]string{}
		for _, key := range []string{"model", "provider", "api_key", "base_url"} {
			if v, ok := fields(key); ok && v != nil {
				if s := strings.TrimSpace(pyrepr.Str(v)); s != "" {
					route[key] = s
				}
			}
		}
		if route["model"] == "" {
			apiLog().Warn(fmt.Sprintf("api_server model_routes: route %s has no 'model'; dropping", pyrepr.Repr(aliasStr)))
			continue
		}
		if _, exists := table.routes[aliasStr]; !exists {
			table.keys = append(table.keys, aliasStr)
		}
		table.routes[aliasStr] = route
	}
	return table
}

// Route entry for a model alias, nil when the value is not a known alias
func (a *APIServerAdapter) resolveRoute(modelAlias any) map[string]string {
	s, ok := modelAlias.(string)
	if !ok {
		return nil
	}
	return a.modelRoutes.get(s)
}

// Model persisted on a session row, empty for none or the virtual alias
func (a *APIServerAdapter) storedSessionModel(session *pyjson.Object) string {
	if session == nil {
		return ""
	}
	stored, _ := session.Get("model")
	if !pyrepr.Truthy(stored) || stored == a.modelName {
		return ""
	}
	return pyrepr.Str(stored)
}

var runtimeIDUnsafeRE = regexp.MustCompile("[\r\n\x00]")

// Trimmed runtime id, empty when too long or carrying line breaks or NUL
func cleanRuntimeID(value any, maxLen int) string {
	text := ""
	if value != nil {
		text = strings.TrimSpace(pyrepr.Str(value))
	}
	if len([]rune(text)) > maxLen || runtimeIDUnsafeRE.MatchString(text) {
		return ""
	}
	return text
}

var providerPrefixRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]{2,64}$`)

// Provider and model of a provider prefixed model id, empty provider when not prefixed
func splitProviderPrefixedModel(model string) (string, string) {
	text := cleanRuntimeID(model, 200)
	if providerName, raw, found := strings.Cut(text, "::"); found {
		if providerPrefixRE.MatchString(providerName) && strings.TrimSpace(raw) != "" {
			return providerName, strings.TrimSpace(raw)
		}
	}
	return "", text
}

// Reasoning config and service tier a model options mapping asks for
func runtimeOptionsFromModelOptions(modelOptions any) *pyjson.Object {
	opts, ok := modelOptions.(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	out := pyjson.NewObject()
	if reasoning, ok := opts.GetOr("reasoning", nil).(*pyjson.Object); ok {
		enabled, _ := reasoning.Get("enabled")
		effort := cleanRuntimeID(reasoning.GetOr("effort", nil), 32)
		switch {
		case enabled == false:
			out.Set("reasoning_config", pyjson.ObjectOf("enabled", false))
		case effort != "":
			out.Set("reasoning_config", pyjson.ObjectOf("enabled", true, "effort", effort))
		case enabled == true:
			out.Set("reasoning_config", pyjson.ObjectOf("enabled", true))
		}
	}
	if tier := cleanRuntimeID(opts.GetOr("service_tier", nil), 32); tier != "" {
		out.Set("service_tier", tier)
	} else if gateway.APICoerceRequestBool(opts.GetOr("fast", nil), false) {
		out.Set("service_tier", "priority")
	}
	return out
}

// Runtime request a session chat body asks for, the virtual alias nulled before any route is built
type runtimeRequest struct {
	requestedProvider, requestedModel, requestedRawModel string
	route                                                map[string]string
	routeSource                                          string
	runtimeOptions                                       *pyjson.Object
	requireModelLock                                     bool
	modelOptions                                         *pyjson.Object
	persistedLock                                        bool
}

// Requested provider and model as the wire mapping
func (r *runtimeRequest) requested() *pyjson.Object {
	var model any
	if r.requestedModel != "" {
		model = r.requestedModel
	}
	return pyjson.ObjectOf("provider", r.requestedProvider, "model", model, "raw_model", r.requestedRawModel)
}

// Runtime request parsed from a session chat body
func (a *APIServerAdapter) sessionRuntimeRequestFromBody(body *pyjson.Object) *runtimeRequest {
	rawModel := cleanRuntimeID(pyrepr.Or(body.GetOr("model", nil), body.GetOr("model_id", nil)), 200)
	rawProvider := cleanRuntimeID(pyrepr.Or(body.GetOr("provider", nil), body.GetOr("provider_id", nil)), 80)
	prefixedProvider, splitModel := splitProviderPrefixedModel(rawModel)
	providerName := rawProvider
	if providerName == "" {
		providerName = prefixedProvider
	}
	model := splitModel
	if model == "" {
		model = rawModel
	}
	aliasRoute := a.resolveRoute(rawModel)
	if aliasRoute == nil {
		aliasRoute = a.resolveRoute(model)
	}
	var route map[string]string
	if aliasRoute != nil {
		route = copyRoute(aliasRoute)
	}
	if model == a.modelName {
		model = ""
	}
	routeSource := "global"
	if route != nil {
		routeSource = "model_routes"
	}
	if route == nil && model != "" {
		route = map[string]string{"model": model}
		if providerName != "" {
			route["provider"] = providerName
		}
		routeSource = "raw_request"
	}
	modelOptions, _ := body.GetOr("model_options", nil).(*pyjson.Object)
	if modelOptions == nil {
		modelOptions = pyjson.NewObject()
	}
	return &runtimeRequest{requestedProvider: providerName, requestedModel: model, requestedRawModel: rawModel,
		route: route, routeSource: routeSource, runtimeOptions: runtimeOptionsFromModelOptions(body.GetOr("model_options", nil)),
		requireModelLock: gateway.APICoerceRequestBool(body.GetOr("require_model_lock", nil), false), modelOptions: modelOptions}
}

func copyRoute(route map[string]string) map[string]string {
	out := make(map[string]string, len(route))
	for k, v := range route {
		out[k] = v
	}
	return out
}

// Model and provider cleaned from a requested or lock mapping
func requestedIDs(requested *pyjson.Object) (string, string) {
	if requested == nil {
		return "", ""
	}
	return cleanRuntimeID(requested.GetOr("model", nil), 200), cleanRuntimeID(requested.GetOr("provider", nil), 80)
}

// 400 or 409 when a required model lock has nothing to route or would fall back to the global model
func (a *APIServerAdapter) runtimeLockError(r *runtimeRequest) *gateway.WebResponse {
	if !r.requireModelLock {
		return nil
	}
	model, providerName := requestedIDs(r.requested())
	if model == "" && providerName == "" {
		return errorResponse("require_model_lock was set but no model/provider was provided", http.StatusBadRequest,
			errOpts{Code: "missing_model"})
	}
	if r.route == nil || r.routeSource == "global" {
		return errorResponse("Requested Browser model lock cannot be routed; refusing silent global fallback",
			http.StatusConflict, errOpts{Code: "model_lock_unavailable"})
	}
	return nil
}

// Persists a newly confirmed model lock, true when nothing needed persisting
func (a *APIServerAdapter) persistSessionRuntimeLock(ctx context.Context, sessionID string, r *runtimeRequest) bool {
	if r.persistedLock || !r.requireModelLock {
		return true
	}
	model, providerName := requestedIDs(r.requested())
	if model == "" && providerName == "" {
		return false
	}
	db := a.ensureSessionDB(ctx)
	if db == nil {
		return false
	}
	var modelArg, providerArg *string
	if model != "" {
		modelArg = &model
	}
	if providerName != "" {
		providerArg = &providerName
	}
	routeSource := r.routeSource
	if err := db.UpdateSessionRuntimeLock(ctx, sessionID, state.RuntimeLock{Model: modelArg, Provider: providerArg,
		ModelOptions: pyjson.Plain(r.modelOptions), RouteSource: &routeSource, Confirmed: r.requireModelLock}); err != nil {
		apiLog().Warn(fmt.Sprintf("[%s] failed to persist session runtime lock for %s", a.Name(), sessionID), "error", err)
		return false
	}
	return true
}

// Session model config as a mapping, a JSON string decoded and anything else empty
func parseSessionModelConfig(raw any) *pyjson.Object {
	switch v := raw.(type) {
	case *pyjson.Object:
		return v.Copy()
	case map[string]any:
		o, _ := pyjson.AsObject(v)
		return o
	case string:
		if strings.TrimSpace(v) != "" {
			if parsed, err := pyjson.Loads(v); err == nil {
				if o, ok := parsed.(*pyjson.Object); ok {
					return o
				}
			}
		}
	}
	return pyjson.NewObject()
}

// Runtime request a confirmed persisted browser model lock pins, nil without one
func (a *APIServerAdapter) runtimeRequestFromPersistedSessionLock(session *pyjson.Object, body *pyjson.Object) *runtimeRequest {
	if session == nil {
		return nil
	}
	modelConfig := parseSessionModelConfig(session.GetOr("model_config", nil))
	lock, ok := modelConfig.GetOr("browser_model_lock", nil).(*pyjson.Object)
	if !ok || !gateway.APICoerceRequestBool(lock.GetOr("confirmed", nil), false) {
		return nil
	}
	model, providerName := requestedIDs(lock)
	if model == "" && providerName == "" {
		return nil
	}
	var route map[string]string
	if strings.ToLower(cleanRuntimeID(lock.GetOr("route_source", nil), 64)) == "model_routes" {
		if model != "" {
			route = a.resolveRoute(model)
		}
	} else {
		route = map[string]string{}
		if model != "" {
			route["model"] = model
		}
		if providerName != "" {
			route["provider"] = providerName
		}
	}
	if len(route) == 0 {
		route = nil
	}
	modelOptions, ok := body.GetOr("model_options", nil).(*pyjson.Object)
	if !ok {
		modelOptions, _ = lock.GetOr("model_options", nil).(*pyjson.Object)
	}
	options := runtimeOptionsFromModelOptions(modelOptions)
	if modelOptions == nil {
		modelOptions = pyjson.NewObject()
	}
	return &runtimeRequest{requestedProvider: providerName, requestedModel: model, requestedRawModel: model, route: route,
		routeSource: "session_model_lock", runtimeOptions: options, requireModelLock: true, modelOptions: modelOptions,
		persistedLock: true}
}

// Body runtime request, else the persisted session lock when the body names no model or provider
func (a *APIServerAdapter) effectiveSessionRuntimeRequest(session, body *pyjson.Object) *runtimeRequest {
	r := a.sessionRuntimeRequestFromBody(body)
	if r.requestedModel != "" || r.requestedProvider != "" {
		return r
	}
	if persisted := a.runtimeRequestFromPersistedSessionLock(session, body); persisted != nil {
		return persisted
	}
	return r
}

// Wire runtime metadata with cleaned ids, the requested pair and lock state when present
func sanitizeRuntimeMetadata(runtime, requestedRuntime *pyjson.Object, routeSource, modelLock string) *pyjson.Object {
	payload := pyjson.NewObject()
	if runtime != nil {
		payload = runtime.Copy()
	}
	if routeSource == "" {
		routeSource = "global"
	}
	providerName := cleanRuntimeID(firstTruthy(payload.GetOr("provider", nil), payload.GetOr("provider_id", nil), payload.GetOr("effective_provider", nil)), 80)
	model := cleanRuntimeID(firstTruthy(payload.GetOr("model", nil), payload.GetOr("model_id", nil), payload.GetOr("effective_model", nil)), 200)
	source := cleanRuntimeID(pyrepr.Or(payload.GetOr("route_source", nil), routeSource), 64)
	if source == "" {
		source = "global"
	}
	result := pyjson.ObjectOf("provider", providerName, "model", model, "route_source", source)
	payloadRequested, _ := payload.GetOr("requested", nil).(*pyjson.Object)
	if (requestedRuntime != nil && requestedRuntime.Len() > 0) || (payloadRequested != nil && payloadRequested.Len() > 0) {
		src := requestedRuntime
		if src == nil || src.Len() == 0 {
			src = payloadRequested
		}
		m, p := requestedIDs(src)
		result.Set("requested", pyjson.ObjectOf("provider", p, "model", m))
	}
	lockValue := any(modelLock)
	if modelLock == "" {
		lockValue = payload.GetOr("model_lock", nil)
	}
	if pyrepr.Truthy(lockValue) {
		result.Set("model_lock", cleanRuntimeID(lockValue, 32))
	}
	return result
}

// Lowercased session source restricted to known surfaces, browser spelled hermes_browser
func normalizeSessionSource(value any) string {
	text := ""
	if pyrepr.Truthy(value) {
		text = strings.ToLower(strings.TrimSpace(pyrepr.Str(value)))
	}
	switch text {
	case "api_server", "hermes_browser", "cli", "telegram", "discord", "slack", "desktop", "dashboard":
		return text
	case "browser":
		return "hermes_browser"
	}
	return "api_server"
}

// The gateway's per session model override for a key, rehydrated from storage first
func (a *APIServerAdapter) sessionModelOverrideFor(ctx context.Context, sessionKey string) (override map[string]any) {
	if sessionKey == "" {
		return nil
	}
	defer func() {
		if recover() != nil {
			override = nil
		}
	}()
	runner, ok := gateway.CurrentGatewayRunner().(gateway.APIServerRunner)
	if !ok || runner == nil {
		return nil
	}
	return runner.SessionModelOverrideFor(ctx, sessionKey)
}

// Conflict text for a request provider that a pinned model route contradicts, empty when unambiguous
func (a *APIServerAdapter) requestRouteConflictError(ctx context.Context, sessionID, gatewaySessionKey, requestedModel, requestedProvider string, route map[string]string) string {
	requestProvider := cleanString(requestedProvider)
	if requestProvider == "" || route == nil {
		return ""
	}
	key := gatewaySessionKey
	if key == "" {
		key = sessionID
	}
	if len(a.sessionModelOverrideFor(ctx, key)) > 0 {
		return ""
	}
	routeProvider := cleanString(route["provider"])
	routeAPIKey := cleanString(route["api_key"])
	routeBaseURL := cleanString(route["base_url"])
	routeAlias := cleanString(requestedModel)
	if routeAlias == "" {
		routeAlias = "requested model"
	}
	if routeProvider != "" && requestProvider != routeProvider {
		return fmt.Sprintf("Model route '%s' is pinned to provider '%s'. Remove 'provider' or use '%s'.", routeAlias, routeProvider, routeProvider)
	}
	if routeProvider == "" && (routeAPIKey != "" || routeBaseURL != "") {
		return fmt.Sprintf("Model route '%s' pins route credentials/base_url. Do not combine it with an explicit 'provider'.", routeAlias)
	}
	return ""
}

// Clean request string of a Go string, empty for blank
func cleanString(value string) string {
	if s := gateway.APICleanRequestString(value); s != nil {
		return *s
	}
	return ""
}

// Runtime kwargs of a provider with the gateway resolver as fallback, a typed auth error when required
func resolveProviderRuntime(ctx context.Context, providerName, targetModel string, required bool) (map[string]any, error) {
	name := cleanString(providerName)
	if name == "" {
		return nil, nil
	}
	kwargs, err := resolveRequestRuntimeAgentKwargs(ctx, name, targetModel)
	if err == nil {
		return kwargs, nil
	}
	fallback, fallbackErr := func() (out map[string]any, ferr error) {
		defer func() {
			if rec := recover(); rec != nil {
				ferr = fmt.Errorf("%v", rec)
			}
		}()
		return gateway.APIAgentRuntime.ResolveRuntimeAgentKwargsForProvider(ctx, name, targetModel)
	}()
	if fallbackErr == nil {
		return fallback, nil
	}
	if required {
		return nil, &providerAuthResolutionError{msg: err.Error(), cause: err}
	}
	apiLog().Debug(fmt.Sprintf("api_server provider-runtime refresh failed for provider=%s model=%s", name, targetModel), "error", err)
	return nil, nil
}

// Merges a provider's runtime into the kwargs, true when one resolved
func applyProviderRuntime(ctx context.Context, runtimeKwargs map[string]any, providerName, targetModel string, required bool) (bool, error) {
	providerRuntime, err := resolveProviderRuntime(ctx, providerName, targetModel, required)
	if err != nil {
		return false, err
	}
	if len(providerRuntime) > 0 {
		gateway.APIApplyRuntimeAgentOverrides(runtimeKwargs, providerRuntime)
	}
	return len(providerRuntime) > 0, nil
}

// Fills an empty model from the provider default or the last known good one, recording good ones
func (a *APIServerAdapter) recoverOrRecordModel(ctx context.Context, model string, runtimeKwargs map[string]any, gatewaySessionKey string) string {
	if model == "" {
		if providerName, _ := runtimeKwargs["provider"].(string); providerName != "" {
			func() {
				defer func() { _ = recover() }()
				model = provider.GetDefaultModelForProvider(ctx, providerName)
				if model != "" {
					apiLog().Info(fmt.Sprintf("No model configured — defaulting to %s for provider %s", model, providerName))
				}
			}()
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if model == "" {
		recovered := a.lastResolvedModel[gatewaySessionKey]
		if recovered == "" {
			recovered = a.lastResolvedModel["*"]
		}
		if recovered != "" && recovered != a.modelName {
			apiLog().Warn(fmt.Sprintf("Empty model resolved for session=%s — recovering last-known-good model %s "+
				"(config read likely returned empty; see #35314)", gatewaySessionKey, recovered))
			model = recovered
		}
	} else if model != a.modelName {
		if gatewaySessionKey != "" {
			a.lastResolvedModel[gatewaySessionKey] = model
		}
		a.lastResolvedModel["*"] = model
	}
	return model
}

// Agent runtime choice after the lock, override, session model, route and request precedence chain
type agentRuntimeSelection struct {
	model, requestModel, requestProvider string
	sessionOverride                      map[string]any
}

// Applies the model and provider precedence chain to the runtime kwargs
func (a *APIServerAdapter) selectAgentRuntime(ctx context.Context, runtimeKwargs map[string]any, model string, o agentOptions) (agentRuntimeSelection, error) {
	requestModel := cleanString(o.requestedModel)
	requestProvider := cleanString(o.requestedProvider)
	routeModel := cleanString(o.route["model"])
	routeProvider := cleanString(o.route["provider"])
	sessionKey := o.gatewaySessionKey
	if sessionKey == "" {
		sessionKey = o.sessionID
	}
	sessionRowModel := cleanString(o.sessionModel)
	currentProvider := ""
	if s, ok := runtimeKwargs["provider"].(string); ok {
		currentProvider = cleanString(s)
	}
	var sessionOverride map[string]any
	if !o.confirmedRuntimeLock {
		sessionOverride = a.sessionModelOverrideFor(ctx, sessionKey)
	}
	switch {
	case len(sessionOverride) > 0:
		model = provider.ResolveEffectiveModel(sessionOverride, nil, model)
		overrideProvider := ""
		if s, ok := sessionOverride["provider"].(string); ok {
			overrideProvider = cleanString(s)
		}
		if overrideProvider == "" {
			overrideProvider = currentProvider
		}
		if _, err := applyProviderRuntime(ctx, runtimeKwargs, overrideProvider, model, false); err != nil {
			return agentRuntimeSelection{}, err
		}
		gateway.APIApplyRuntimeAgentOverrides(runtimeKwargs, sessionOverride)
		if o.route != nil || requestModel != "" || requestProvider != "" {
			apiLog().Debug("api_server request selection skipped: session /model override wins for " + sessionKey)
		}
	case sessionRowModel != "" && !o.confirmedRuntimeLock:
		if _, err := applyProviderRuntime(ctx, runtimeKwargs, currentProvider, sessionRowModel, false); err != nil {
			return agentRuntimeSelection{}, err
		}
		model = provider.ResolveEffectiveModel(nil, sessionRowModel, model)
		if requestModel != "" || requestProvider != "" {
			apiLog().Debug("api_server request selection skipped: session-persisted model wins for " + sessionKey)
		}
	default:
		effectiveModel := requestModel
		if o.route != nil {
			effectiveModel = routeModel
		}
		if effectiveModel == "" {
			effectiveModel = model
		}
		effectiveProvider := requestProvider
		if effectiveProvider == "" {
			effectiveProvider = routeProvider
		}
		if effectiveProvider == "" {
			effectiveProvider = currentProvider
		}
		applied := false
		if effectiveProvider != "" && (requestProvider != "" || routeProvider != "" || effectiveModel != model) {
			var err error
			applied, err = applyProviderRuntime(ctx, runtimeKwargs, effectiveProvider, effectiveModel,
				requestProvider != "" || o.confirmedRuntimeLock)
			if err != nil {
				return agentRuntimeSelection{}, err
			}
		}
		if !applied && effectiveProvider != "" && effectiveProvider != currentProvider {
			runtimeKwargs["provider"] = effectiveProvider
		}
		model = effectiveModel
		for _, key := range []string{"api_key", "base_url"} {
			if value := cleanString(o.route[key]); value != "" {
				runtimeKwargs[key] = value
			}
		}
		if o.route != nil {
			apiLog().Debug(fmt.Sprintf("api_server request selection applied: model=%s provider=%v route_provider=%s request_provider=%s",
				model, runtimeKwargs["provider"], routeProvider, requestProvider))
		}
	}
	model = a.recoverOrRecordModel(ctx, model, runtimeKwargs, o.gatewaySessionKey)
	return agentRuntimeSelection{model: model, sessionOverride: sessionOverride, requestModel: requestModel,
		requestProvider: requestProvider}, nil
}

// Keyword arguments of one API agent build
type agentOptions struct {
	ephemeralSystemPrompt  string
	sessionID              string
	streamDeltaCallback    any
	toolProgressCallback   any
	toolStartCallback      any
	toolCompleteCallback   any
	interimAssistantCb     any
	reasoningCallback      any
	statusCallback         any
	gatewaySessionKey      string
	requestedModel         string
	requestedProvider      string
	modelOptions           any
	route                  map[string]string
	sessionModel           string
	confirmedRuntimeLock   bool
	roomDispatch           *pyjson.Object
	roomExecutionPolicy    *pyjson.Object
	hasRoomDispatch        bool
	hasRoomExecutionPolicy bool
	env                    *tools.Env
}

// API runtime metadata of an agent the adapter built, the hermes api runtime attribute
type agentRuntimeInfo struct {
	provider, model, routeSource string
}

var agentRuntimes = struct {
	m map[*agent.AIAgent]agentRuntimeInfo
}{m: map[*agent.AIAgent]agentRuntimeInfo{}}

// Recorded API runtime of an agent, ok false when the adapter did not build it
func apiRuntimeOf(a *agent.AIAgent) (agentRuntimeInfo, bool) {
	turnProcessEpochMu.Lock()
	defer turnProcessEpochMu.Unlock()
	info, ok := agentRuntimes.m[a]
	return info, ok
}

// Forgets the bookkeeping the adapter attached to an agent
func forgetAgent(a *agent.AIAgent) {
	turnProcessEpochMu.Lock()
	defer turnProcessEpochMu.Unlock()
	delete(agentRuntimes.m, a)
}

// Real agent built from the gateway runtime config and the api_server platform toolsets
func (a *APIServerAdapter) createAgent(ctx context.Context, o agentOptions) (*agent.AIAgent, error) {
	runtimeKwargs, err := gateway.APIAgentRuntime.ResolveRuntimeAgentKwargs(ctx)
	if err != nil {
		return nil, &providerAuthResolutionError{msg: err.Error(), cause: err}
	}
	model, _ := runtimeKwargs["model"].(string)
	delete(runtimeKwargs, "model")
	if model == "" {
		model = gateway.ResolveGatewayModel(ctx, nil)
	}
	delete(runtimeKwargs, "_fallback_notice")
	requestReasoningConfig := gateway.APIRequestReasoningConfig(o.modelOptions)
	requestServiceTier := gateway.APIRequestServiceTier(o.modelOptions)
	sel, err := a.selectAgentRuntime(ctx, runtimeKwargs, model, o)
	if err != nil {
		return nil, err
	}
	model = sel.model
	userConfig := gateway.LoadGatewayConfigMap(ctx, "")
	enabledSet := tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), config.Tree(userConfig), "api_server",
		tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	enabledToolsets := make([]string, 0, len(enabledSet))
	for name := range enabledSet {
		enabledToolsets = append(enabledToolsets, name)
	}
	sort.Strings(enabledToolsets)
	interim := o.interimAssistantCb
	if !pyrepr.Truthy(config.ResolveDisplaySetting(ctx, userConfig, "api_server", "interim_assistant_messages", true)) {
		interim = nil
	}
	maxIterations := gateway.CurrentMaxIterations(ctx)
	if o.hasRoomDispatch {
		policy, err := gateway.RoomExecutionPolicyFromMapping(o.roomExecutionPolicy)
		if err != nil {
			return nil, err
		}
		enabledToolsets = append([]string(nil), policy.EnabledToolsets...)
		maxIterations = int(policy.MaxIterations)
	}
	if requestReasoningConfig == nil {
		requestReasoningConfig = gateway.APIAgentRuntime.LoadReasoningConfig(ctx, model)
	}
	checkpoint := gateway.CheckpointAgentKwargs(userConfig)
	var fallback []map[string]any
	if !o.confirmedRuntimeLock {
		fallback = gateway.APIAgentRuntime.LoadFallbackModel(ctx)
	}
	var manager *memory.MemoryManager
	if checked := a.memorySessions.Checkout(ctx, o.sessionID); checked != nil {
		manager = checked
	}
	opts := agent.AIAgentOptions{
		Model: model, MaxIterations: maxIterations, QuietMode: true, VerboseLogging: false,
		EphemeralSystemPrompt: o.ephemeralSystemPrompt, EnabledToolsets: enabledToolsets, SessionID: o.sessionID,
		Platform: "api_server", StreamDeltaCallback: o.streamDeltaCallback, ToolProgressCallback: o.toolProgressCallback,
		ToolStartCallback: o.toolStartCallback, ToolCompleteCallback: o.toolCompleteCallback, InterimAssistantCallback: interim,
		ReasoningCallback: o.reasoningCallback, StatusCallback: o.statusCallback, SessionDB: a.ensureSessionDB(ctx),
		FallbackModel: fallback, ReasoningConfig: requestReasoningConfig, GatewaySessionKey: o.gatewaySessionKey,
		MemoryManager: manager, Env: o.env,
	}
	applyRuntimeKwargs(&opts, runtimeKwargs)
	applyCheckpointKwargs(&opts, checkpoint)
	if requestServiceTier != gateway.APIRequestOptionMissing {
		opts.ServiceTier = pyrepr.Str(requestServiceTier)
		if requestServiceTier == nil {
			opts.ServiceTier = ""
		}
	}
	built, err := agent.NewAIAgent(ctx, opts)
	if err != nil {
		return nil, err
	}
	routeSource := "global"
	switch {
	case o.confirmedRuntimeLock:
		routeSource = "session_model_lock"
	case len(sel.sessionOverride) > 0:
		routeSource = "session_model_override"
	case o.route != nil || sel.requestModel != "" || sel.requestProvider != "":
		routeSource = "raw_request"
	}
	providerName, _ := runtimeKwargs["provider"].(string)
	if providerName == "" {
		providerName = built.Provider()
	}
	runtimeModel := built.Model()
	if runtimeModel == "" {
		runtimeModel = model
	}
	turnProcessEpochMu.Lock()
	agentRuntimes.m[built] = agentRuntimeInfo{provider: providerName, model: runtimeModel, routeSource: routeSource}
	turnProcessEpochMu.Unlock()
	return built, nil
}

// Copies provider runtime kwargs onto the agent options the way the keyword spread does
func applyRuntimeKwargs(opts *agent.AIAgentOptions, kwargs map[string]any) {
	str := func(key string) string {
		if s, ok := kwargs[key].(string); ok {
			return s
		}
		return ""
	}
	opts.APIKey, opts.BaseURL, opts.Provider, opts.APIMode, opts.Command = str("api_key"), str("base_url"), str("provider"), str("api_mode"), str("command")
	opts.RequestedProvider = str("requested_provider")
	switch args := kwargs["args"].(type) {
	case []string:
		opts.Args = append([]string(nil), args...)
	case []any:
		for _, v := range args {
			opts.Args = append(opts.Args, pyrepr.Str(v))
		}
	}
	if pool, ok := kwargs["credential_pool"].(*provider.Pool); ok {
		opts.CredentialPool = pool
	}
	if overrides, ok := kwargs["request_overrides"].(map[string]any); ok {
		opts.RequestOverrides = overrides
	}
	if caps, ok := kwargs["capabilities"].(map[string]any); ok {
		opts.Capabilities = map[string]bool{}
		for k, v := range caps {
			opts.Capabilities[k] = pyrepr.Truthy(v)
		}
	}
}

// Copies the checkpoint config kwargs onto the agent options
func applyCheckpointKwargs(opts *agent.AIAgentOptions, kwargs map[string]any) {
	opts.CheckpointsEnabled = pyrepr.Truthy(kwargs["checkpoints_enabled"])
	intOf := func(key string) int {
		n, err := pyint.Of(kwargs[key])
		if err != nil || !n.IsInt64() {
			return 0
		}
		return int(n.Int64())
	}
	opts.CheckpointMaxSnapshots = intOf("checkpoint_max_snapshots")
	opts.CheckpointMaxTotalSizeMB = intOf("checkpoint_max_total_size_mb")
	opts.CheckpointMaxFileSizeMB = intOf("checkpoint_max_file_size_mb")
}
