package provider

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Provider, model, base URL, key and api mode an auxiliary task resolved to, empty strings for None
type auxTaskRoute struct {
	Provider, Model, BaseURL string
	APIKey                   any
	APIMode                  string
}

// Stand in a callable explicit key travels under through the string typed resolver
const auxCallableKeySentinel = "\x00icebath-callable-api-key\x00"

// Provider, model, base URL, key and api mode for a task, explicit args beating config beating auto
func auxResolveTaskProviderModel(ctx context.Context, task any, provider, model, baseURL string, apiKey any) auxTaskRoute {
	var cfg map[string]any
	if err := auxRecoverRaise(func() { cfg, _ = auxLoadConfigReadonlyFn(ctx) }); err != nil {
		cfg = nil
	}
	keyText, _ := apiKey.(string)
	callable := apiKey != nil && reflect.TypeOf(apiKey).Kind() == reflect.Func
	if callable {
		keyText = auxCallableKeySentinel
	}
	route := newResolver(ctx, cfg, nil, "", nil).resolveTaskProviderModel(task, provider, model, baseURL, keyText)
	out := auxTaskRoute{Provider: route.provider, Model: route.model, BaseURL: route.baseURL, APIMode: string(route.apiMode)}
	switch {
	case callable && route.apiKey == auxCallableKeySentinel:
		out.APIKey = apiKey
	case route.apiKey != "":
		out.APIKey = route.apiKey
	}
	return out
}

// Configured provider of a task with no explicit args, auto when nothing is configured
func ResolveAuxTaskProvider(ctx context.Context, task string) string {
	return auxResolveTaskProviderModel(ctx, task, "", "", "", nil).Provider
}

// Explicit non reasoning compression route
type CompressionFastLane struct {
	CertifiedNonReasoning bool
	ReasoningConfig       map[string]any
}

// Provider, model and whether reasoning is explicitly disabled in a task config
func auxFastLaneConfigFields(config map[string]any) (string, string, bool) {
	provider := strings.ToLower(strings.TrimSpace(auxPyStrOr(config["provider"], "")))
	model := strings.TrimSpace(auxPyStrOr(config["model"], ""))
	parsed := ParseReasoningEffort(config["reasoning_effort"])
	nonReasoning := false
	if parsed != nil {
		enabled, isBool := parsed["enabled"].(bool)
		nonReasoning = isBool && !enabled
	}
	return provider, model, nonReasoning
}

// ResolveCompressionFastLane certifies explicit non reasoning settings only on the matching destination
func ResolveCompressionFastLane(ctx context.Context, actualProvider, actualModel, requestedProvider, requestedModel string, routeConfig map[string]any) (CompressionFastLane, error) {
	config := routeConfig
	if config == nil {
		var err error
		if config, err = auxGetAuxiliaryTaskConfig(ctx, "compression"); err != nil {
			return CompressionFastLane{}, err
		}
	}
	cfgProvider, cfgModel, nonReasoning := auxFastLaneConfigFields(config)
	provider := strings.ToLower(strings.TrimSpace(requestedProvider))
	if provider == "" {
		provider = cfgProvider
	}
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = cfgModel
	}
	explicitRoute := provider != "" && provider != "auto" && strings.ToLower(model) != "" && strings.ToLower(model) != "auto"
	actualNorm := auxNormalizeAuxProvider(ctx, auxFallbackProviderFromLabel(actualProvider))
	providerMatches := actualNorm == auxNormalizeAuxProvider(ctx, provider)
	modelMatches := strings.ToLower(strings.TrimSpace(actualModel)) == strings.ToLower(model)
	if explicitRoute && providerMatches && modelMatches && nonReasoning {
		return CompressionFastLane{CertifiedNonReasoning: true, ReasoningConfig: map[string]any{"enabled": false, "effort": "none"}}, nil
	}
	return CompressionFastLane{}, nil
}

// True when a task config declares fast only controls that cannot leak
func auxCompressionConfigClaimsFastLane(config map[string]any) bool {
	provider, model, nonReasoning := auxFastLaneConfigFields(config)
	lower := strings.ToLower(model)
	return provider != "" && provider != "auto" && lower != "" && lower != "auto" && nonReasoning
}

// Certified compression controls applied to one resolved route, unchanged for other tasks or an explicit cap
func auxCompressionFastLaneControls(ctx context.Context, task, actualProvider, actualModel, requestedProvider, requestedModel string, routeConfig, leakGuardConfig map[string]any, maxTokens any, extraBody map[string]any) (any, map[string]any, error) {
	if task != "compression" || maxTokens != nil {
		return maxTokens, extraBody, nil
	}
	body := auxCloneMap(extraBody)
	if body == nil {
		body = map[string]any{}
	}
	lane, err := ResolveCompressionFastLane(ctx, actualProvider, actualModel, requestedProvider, requestedModel, routeConfig)
	if err != nil {
		return nil, nil, err
	}
	if lane.ReasoningConfig != nil {
		if _, ok := body["reasoning"]; !ok {
			body["reasoning"] = lane.ReasoningConfig
		}
	} else if auxCompressionConfigClaimsFastLane(leakGuardConfig) {
		delete(body, "reasoning")
	}
	return maxTokens, body, nil
}

var auxFallbackLabelRe = regexp.MustCompile(`^(?:fallback_chain\[\d+\]|fallback_providers\[\d+\]|main-agent)\(([^)]+)\)$`)

// Provider identifier recovered from a fallback display label
func auxFallbackProviderFromLabel(label string) string {
	if m := auxFallbackLabelRe.FindStringSubmatch(label); m != nil {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(label)
}

// Custom base URL normalized for entry matching, empty for a non string
func normalizedCustomBaseURL(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// True when an entry's model or model catalog matches the agent model
func customProviderModelMatches(agentModel string, entry map[string]any) bool {
	norm := strings.ToLower(strings.TrimSpace(agentModel))
	var catalog []string
	switch models := entry["models"].(type) {
	case map[string]any:
		for k := range models {
			catalog = append(catalog, strings.ToLower(strings.TrimSpace(k)))
		}
	case []any:
		for _, m := range models {
			catalog = append(catalog, strings.ToLower(strings.TrimSpace(pyrepr.Str(m))))
		}
	case []string:
		for _, m := range models {
			catalog = append(catalog, strings.ToLower(strings.TrimSpace(m)))
		}
	}
	for _, c := range catalog {
		if c == norm {
			return true
		}
	}
	providerModel := strings.ToLower(strings.TrimSpace(auxPyStrOr(entry["model"], "")))
	return (providerModel == "" && len(catalog) == 0) || providerModel == norm
}

// CustomProviderExtraBodyForAgent is the extra_body of the custom entry serving a custom route, model pinned entries first
func CustomProviderExtraBodyForAgent(provider, model, baseURL string, customProviders []map[string]any) map[string]any {
	providerNorm := strings.ToLower(strings.TrimSpace(provider))
	if providerNorm != "custom" && !strings.HasPrefix(providerNorm, "custom:") {
		return nil
	}
	_, keyFilter, _ := strings.Cut(providerNorm, ":")
	keyFilter = strings.TrimSpace(keyFilter)
	target := normalizedCustomBaseURL(baseURL)
	if target == "" {
		return nil
	}
	var fallback map[string]any
	for _, entry := range customProviders {
		if entry == nil {
			continue
		}
		if keyFilter != "" {
			pk := strings.ToLower(strings.TrimSpace(auxPyStrOr(entry["provider_key"], "")))
			name := strings.ToLower(strings.TrimSpace(auxPyStrOr(entry["name"], "")))
			if keyFilter != pk && keyFilter != name {
				continue
			}
		}
		if normalizedCustomBaseURL(entry["base_url"]) != target {
			continue
		}
		extra, ok := entry["extra_body"].(map[string]any)
		if !ok || len(extra) == 0 {
			continue
		}
		if strings.TrimSpace(auxPyStrOr(entry["model"], "")) != "" {
			if customProviderModelMatches(model, entry) {
				return auxCloneMap(extra)
			}
		} else if fallback == nil {
			fallback = auxCloneMap(extra)
		}
	}
	return fallback
}

// Destination custom entry extra_body layered under the task and caller body, unchanged off a custom route
func auxWithCustomEndpointExtraBody(ctx context.Context, extraBody map[string]any, provider, model, baseURL string) map[string]any {
	if baseURL == "" {
		return extraBody
	}
	var inherited map[string]any
	if err := auxRecoverRaise(func() {
		cfg, err := auxLoadConfigReadonlyFn(ctx)
		if err != nil {
			panic(err)
		}
		inherited = CustomProviderExtraBodyForAgent(provider, model, baseURL, CompatibleCustomProviders(cfg))
	}); err != nil {
		slog.Debug("custom provider extra_body lookup failed for aux request", "error", err)
		return extraBody
	}
	if len(inherited) == 0 {
		return extraBody
	}
	out := auxCloneMap(inherited)
	for k, v := range extraBody {
		out[k] = v
	}
	return out
}

// Copy of auxiliary task extra_body with reasoning_effort folded into reasoning unless one is configured
func auxGetTaskExtraBody(ctx context.Context, task string) (map[string]any, error) {
	tc, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	if raw, ok := tc["extra_body"].(map[string]any); ok {
		result = auxCloneMap(raw)
	}
	if _, ok := result["reasoning"]; ok {
		return result, nil
	}
	effort := tc["reasoning_effort"]
	if effort == nil || effort == "" {
		return result, nil
	}
	if task == "moa_reference" || task == "moa_aggregator" {
		slog.Warn(fmt.Sprintf("auxiliary.%s.reasoning_effort is not supported — MoA reasoning depth is per-slot: set reasoning_effort "+
			"on the preset's reference_models entries / aggregator instead (moa.presets.<name>...). Ignoring.", task))
		return result, nil
	}
	if parsed := ParseReasoningEffort(effort); parsed != nil {
		result["reasoning"] = parsed
	} else {
		slog.Warn(fmt.Sprintf("auxiliary.%s.reasoning_effort %s is not a valid level (none, minimal, low, medium, high, xhigh, max, ultra) — ignoring", task, pyrepr.Repr(effort)))
	}
	return result, nil
}

// Bounded counting semaphore, releasing past its limit an error
type auxSemaphore struct {
	slots chan struct{}
	limit int
}

func newAuxSemaphore(limit int) *auxSemaphore {
	return &auxSemaphore{slots: make(chan struct{}, limit), limit: limit}
}

// Takes a slot, waiting until one frees or the context ends
func (s *auxSemaphore) Acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Frees a slot, an error when more slots are freed than were taken
func (s *auxSemaphore) Release() error {
	select {
	case <-s.slots:
		return nil
	default:
		return &ValueError{Message: "Semaphore released too many times"}
	}
}

// Semaphore slots cached per profile home and task with the limit they were built for
type auxSemKey struct{ Home, Task string }

type auxSemEntry struct {
	limit int
	sem   *auxSemaphore
}

var (
	auxSemMu           sync.Mutex
	auxSyncSemaphores  = map[auxSemKey]auxSemEntry{}
	auxAsyncSemaphores = map[any]map[auxSemKey]auxSemEntry{}
)

// Positive max_concurrency of a task, false when unset, malformed or the vision task
func auxGetTaskMaxConcurrency(ctx context.Context, task string) (int, bool, error) {
	if task == "" || task == "vision" {
		return 0, false, nil
	}
	tc, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil {
		return 0, false, err
	}
	value, err := auxPyInt(tc["max_concurrency"])
	if err != nil || value <= 0 {
		return 0, false, nil
	}
	return value, true, nil
}

// Cached semaphore for a key rebuilt when the limit changed, the caller holding the semaphore lock
func auxCachedSemaphoreLocked(store map[auxSemKey]auxSemEntry, key auxSemKey, limit int) *auxSemaphore {
	entry, ok := store[key]
	if !ok || entry.limit != limit {
		entry = auxSemEntry{limit: limit, sem: newAuxSemaphore(limit)}
		store[key] = entry
	}
	return entry.sem
}

// Per task sync semaphore of the profile, nil when the task has no concurrency limit
func auxAcquireSyncAuxSemaphore(ctx context.Context, task string) (*auxSemaphore, error) {
	limit, ok, err := auxGetTaskMaxConcurrency(ctx, task)
	if err != nil || !ok {
		return nil, err
	}
	auxSemMu.Lock()
	defer auxSemMu.Unlock()
	return auxCachedSemaphoreLocked(auxSyncSemaphores, auxSemKey{home.Key(home.Dir(ctx)), task}, limit), nil
}

// Per task semaphore of the bound event loop, nil without a limit or a running loop
func auxAcquireAsyncAuxSemaphore(ctx context.Context, task string) (*auxSemaphore, error) {
	limit, ok, err := auxGetTaskMaxConcurrency(ctx, task)
	if err != nil || !ok {
		return nil, err
	}
	loop := auxCurrentEventLoop(ctx)
	if loop == nil {
		return nil, nil
	}
	tables := LiveLoopTable(auxAsyncSemaphores, loop, &auxSemMu, auxSameObject, auxLoopClosed,
		func() map[auxSemKey]auxSemEntry { return map[auxSemKey]auxSemEntry{} })
	auxSemMu.Lock()
	defer auxSemMu.Unlock()
	return auxCachedSemaphoreLocked(tables, auxSemKey{home.Key(home.Dir(ctx)), task}, limit), nil
}

// Whether a table key is a bound loop that has closed, so its table can never run again
func auxLoopClosed(other any) bool {
	b, ok := other.(auxBoundLoop)
	return ok && b.IsClosed()
}

// Drops every cached semaphore
func auxResetAuxSemaphores() {
	auxSemMu.Lock()
	defer auxSemMu.Unlock()
	clear(auxSyncSemaphores)
	clear(auxAsyncSemaphores)
}

// True when a profile payload carries a reasoning wire control at any depth
func auxContainsProfileReasoningFields(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, nested := range m {
		if auxProfileReasoningKeys[strings.ToLower(strings.TrimSpace(key))] || auxContainsProfileReasoningFields(nested) {
			return true
		}
	}
	return false
}

var auxNousProviderNames = map[string]bool{"nous": true, "nous-portal": true, "nousresearch": true}

// True when a Nous Portal route serves the model over the Messages wire
func auxNousOnMessagesWire(ctx context.Context, providerNorm, model string) bool {
	if !auxNousProviderNames[providerNorm] {
		return false
	}
	var cfg map[string]any
	_ = auxRecoverRaise(func() { cfg, _ = auxLoadConfigReadonlyFn(ctx) })
	return string(NousMode(model, newResolver(ctx, cfg, nil, "", nil).nousAnthropicWire())) == "anthropic_messages"
}

var (
	auxNvidiaProviderNames       = map[string]bool{"nvidia": true, "nvidia-nim": true, "nim": true, "build-nvidia": true, "nemotron": true}
	auxGeminiNativeProviderNames = map[string]bool{"gemini": true, "google": true, "google-gemini": true, "google-ai-studio": true}
)

// IsGeminiNativeProviderName reports a provider name served by the native Gemini client
func IsGeminiNativeProviderName(name string) bool { return auxGeminiNativeProviderNames[name] }

// Gemini native by provider name, else by base URL shape
func auxIsGeminiNativeRoute(providerNorm, effectiveBase string) bool {
	if auxGeminiNativeProviderNames[providerNorm] {
		return true
	}
	if effectiveBase == "" {
		return false
	}
	native := false
	if err := auxRecoverRaise(func() { native = IsNativeGeminiBaseURL(effectiveBase) }); err != nil {
		return false
	}
	return native
}

// True when an explicit max_tokens is forwarded on the route where it is mandatory or honored
func auxForwardsMaxTokens(ctx context.Context, provider, providerNorm, model, effectiveBase, task string) bool {
	return auxIsAnthropicCompatEndpoint(provider, effectiveBase) ||
		auxNousOnMessagesWire(ctx, providerNorm, model) ||
		auxNvidiaProviderNames[providerNorm] ||
		baseURLHostMatches(effectiveBase, "integrate.api.nvidia.com") ||
		task == "moa_reference" ||
		auxIsGeminiNativeRoute(providerNorm, effectiveBase) ||
		providerNorm == "openrouter" ||
		baseURLHostMatches(effectiveBase, "openrouter.ai") ||
		auxIsManagedLocalEndpoint(effectiveBase)
}

// Tools with duplicate function names dropped and warned about
func auxDedupeToolNames(tools []any, provider, model string) []any {
	seen := map[string]bool{}
	deduped := []any{}
	for _, tool := range tools {
		name := ""
		if t, ok := tool.(map[string]any); ok {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if name != "" && seen[name] {
			slog.Warn(fmt.Sprintf("_build_call_kwargs: duplicate tool name '%s' removed (provider=%s model=%s)", name, provider, model))
			continue
		}
		if name != "" {
			seen[name] = true
		}
		deduped = append(deduped, tool)
	}
	return deduped
}

// Provider profile projection of a request, partial when the projection raised
type auxProfileProjection struct {
	Body             map[string]any
	ReasoningExtra   map[string]any
	TopLevel         map[string]any
	HandlesReasoning bool
	MessagesWire     bool
}

// True when a profile less provider name is a configured OpenAI compatible custom endpoint
func auxRoutesToCustomEndpoint(ctx context.Context, providerNorm string) bool {
	name := auxNormalizeAuxProvider(ctx, providerNorm)
	if name == "custom" {
		return true
	}
	cfg, err := auxLoadConfigFn(ctx)
	if err != nil {
		panic(err)
	}
	return newResolver(ctx, cfg, nil, "", nil).HasNamedCustomProvider(name)
}

// Provider profile extra_body and kwargs projection, whatever was built before a raise kept
func auxProjectProviderProfile(ctx context.Context, provider, providerNorm, model, effectiveBase string, reasoningConfig map[string]any, dataUse map[string]bool) auxProfileProjection {
	p := auxProfileProjection{Body: map[string]any{}, ReasoningExtra: map[string]any{}, TopLevel: map[string]any{}}
	if err := auxRecoverRaise(func() {
		profile, found := auxGetProviderProfileFn(ctx, providerNorm)
		if !found && auxRoutesToCustomEndpoint(ctx, providerNorm) {
			profile, found = auxGetProviderProfileFn(ctx, "custom")
		}
		if !found {
			return
		}
		p.MessagesWire = string(profile.Mode) == "anthropic_messages"
		reasoning := reasoningPtrOfDict(reasoningConfig)
		bodyCtx := map[string]any{"model": model, "base_url": effectiveBase, "reasoning_config": reasoning, "data_use": dataUse, hookCtxKey: ctx}
		if body := profile.BuildExtraBody("", bodyCtx); body != nil {
			p.Body = body
		}
		extrasCtx := map[string]any{"reasoning_config": reasoning, "supports_reasoning": reasoningConfig != nil, "model": model, "base_url": effectiveBase, hookCtxKey: ctx}
		extra, top := profile.BuildAPIKwargsExtras(reasoning, extrasCtx)
		if extra != nil {
			p.ReasoningExtra = extra
		}
		if top != nil {
			p.TopLevel = top
		}
		p.HandlesReasoning = profile.BuildAPIKwargsExtrasHook != nil ||
			auxContainsProfileReasoningFields(p.Body) ||
			auxContainsProfileReasoningFields(p.ReasoningExtra) ||
			auxContainsProfileReasoningFields(p.TopLevel)
	}); err != nil {
		slog.Debug(fmt.Sprintf("_build_call_kwargs: provider profile projection failed for %s: %v", provider, err))
	}
	return p
}

// OpenAI compatible extra_body fallback for a route whose profile declares no reasoning shape
func auxGenericNestedReasoning(reasoningConfig map[string]any) map[string]any {
	if len(reasoningConfig) == 0 {
		return map[string]any{}
	}
	if enabled, ok := reasoningConfig["enabled"].(bool); ok && !enabled {
		return map[string]any{"reasoning": map[string]any{"enabled": false}}
	}
	effort := reasoningConfig["effort"]
	if !auxTruthy(effort) {
		effort = "medium"
	}
	return map[string]any{"reasoning": map[string]any{"enabled": true, "effort": effort}}
}

// Caller extra_body merged with the profile body and reasoning, the generic reasoning fallback and Nous tags
func auxMergeAuxExtraBody(ctx context.Context, extraBody map[string]any, projection auxProfileProjection, reasoningConfig map[string]any, providerNorm string) map[string]any {
	merged := auxCloneMap(extraBody)
	if merged == nil {
		merged = map[string]any{}
	}
	callerReasoning := map[string]any{}
	for k, v := range merged {
		lk := strings.ToLower(strings.TrimSpace(k))
		if auxProfileReasoningKeys[lk] && lk != "reasoning" {
			callerReasoning[k] = v
		}
	}
	if enabled, ok := reasoningConfig["enabled"].(bool); reasoningConfig != nil && ok && !enabled {
		delete(merged, "reasoning")
	}
	for _, part := range []map[string]any{projection.Body, projection.ReasoningExtra, callerReasoning} {
		for k, v := range part {
			merged[k] = v
		}
	}
	if !projection.HandlesReasoning {
		for k, v := range auxGenericNestedReasoning(reasoningConfig) {
			merged[k] = v
		}
	}
	if r, ok := merged["reasoning"].(map[string]any); ok {
		merged["reasoning"] = clampReasoningConfigDict(r, openAICompatWireEfforts)
	}
	if auxNousProviderNames[providerNorm] {
		if _, ok := merged["tags"]; !ok {
			merged["tags"] = nousPortalTags(ctx, "")
		}
		if _, ok := merged["session_id"]; !ok {
			if sticky := portalConversationContext(ctx); sticky != "" {
				merged["session_id"] = sticky
			}
		}
	}
	return merged
}

// Options of a chat completions kwargs build, nil and empty fields for None
type auxCallKwargsOptions struct {
	Temperature       *float64
	MaxTokens         any
	Tools             any
	Timeout           float64
	ExtraBody         map[string]any
	ReasoningConfig   map[string]any
	BaseURL           string
	Task              string
	NoProgressTimeout *float64
}

// Chat completions create kwargs with the model, provider, reasoning and affinity adjustments applied
func auxBuildCallKwargs(ctx context.Context, provider, model string, messages any, opts auxCallKwargsOptions) (map[string]any, error) {
	kwargs := map[string]any{"model": model, "messages": messages, "timeout": opts.Timeout}
	if opts.NoProgressTimeout != nil {
		kwargs["no_progress_timeout"] = *opts.NoProgressTimeout
	}
	effectiveBase := opts.BaseURL
	if effectiveBase == "" && provider == "custom" {
		effectiveBase = auxCurrentCustomBaseURL(ctx)
	}
	extraBody := auxWithCustomEndpointExtraBody(ctx, opts.ExtraBody, provider, model, effectiveBase)
	temperature := opts.Temperature
	omit, fixed, err := auxFixedTemperatureForModel(model, effectiveBase, provider)
	if err != nil {
		return nil, err
	}
	if omit {
		temperature = nil
	} else if fixed != nil {
		temperature = fixed
	}
	if temperature != nil && !anthForbidsSamplingParams(model) {
		kwargs["temperature"] = *temperature
	}
	providerNorm := strings.ToLower(strings.TrimSpace(provider))
	if opts.MaxTokens != nil && auxForwardsMaxTokens(ctx, provider, providerNorm, model, effectiveBase, opts.Task) {
		value, err := auxPyInt(opts.MaxTokens)
		if err != nil {
			return nil, err
		}
		for k, v := range AuxiliaryMaxTokensParam(ctx, value, model) {
			kwargs[k] = v
		}
	}
	var tools []any
	switch t := opts.Tools.(type) {
	case []any:
		tools = t
	case []map[string]any:
		for _, tool := range t {
			tools = append(tools, tool)
		}
	}
	if len(tools) > 0 {
		kwargs["tools"] = auxDedupeToolNames(tools, provider, model)
	}
	reasoningConfig := opts.ReasoningConfig
	if extraBody != nil {
		if task, ok := extraBody["reasoning"].(map[string]any); ok {
			if _, has := task["enabled"]; has {
				extraBody = auxCloneMap(extraBody)
				delete(extraBody, "reasoning")
				if reasoningConfig == nil {
					reasoningConfig = task
				}
			}
		}
	}
	var floored any
	if reasoningConfig != nil {
		floored = reasoningConfig
	}
	floored, err = KnownReasoningFloor(ctx, floored, providerNorm, effectiveBase, model, opts.Task)
	if err != nil {
		return nil, err
	}
	reasoningConfig, _ = floored.(map[string]any)
	reasoningConfig = clampReasoningConfigDict(reasoningConfig, openAICompatWireEfforts)
	projection := auxProjectProviderProfile(ctx, provider, providerNorm, model, effectiveBase, reasoningConfig, auxDataUseRequirement(messages))
	for k, v := range projection.TopLevel {
		kwargs[k] = v
	}
	merged := auxMergeAuxExtraBody(ctx, extraBody, projection, reasoningConfig, providerNorm)
	if _, ok := merged["response_format"]; ok {
		if merged, err = WithoutUnsupportedResponseFormat(ctx, merged, providerNorm, effectiveBase, model, opts.Task); err != nil {
			return nil, err
		}
	}
	if len(merged) > 0 {
		kwargs["extra_body"] = merged
	}
	if len(reasoningConfig) > 0 {
		rawBase := opts.BaseURL
		speaks, err := auxEndpointSpeaksAnthropicMessages(rawBase)
		if err != nil {
			return nil, err
		}
		if providerNorm == "anthropic" || projection.MessagesWire || auxNousOnMessagesWire(ctx, providerNorm, model) || speaks || auxIsAnthropicCompatEndpoint(providerNorm, rawBase) {
			kwargs["_reasoning_config"] = auxCloneMap(reasoningConfig)
		}
	}
	sessionID, _ := auxRuntimeMainValue(ctx, "session_id").(string)
	return MergeSessionAffinityHeaders(ctx, kwargs, provider, opts.BaseURL, sessionID), nil
}

// Union of data_use over messages, what the receiving model must guarantee
func auxDataUseRequirement(messages any) map[string]bool {
	out := map[string]bool{}
	list, _ := messages.([]any)
	if dicts, ok := messages.([]map[string]any); ok {
		for _, m := range dicts {
			list = append(list, m)
		}
	}
	for _, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch v := m["data_use"].(type) {
		case []any:
			for _, r := range v {
				out[pyrepr.Str(r)] = true
			}
		case []string:
			for _, r := range v {
				out[r] = true
			}
		}
	}
	return out
}

// Response with a choices[0].message shape after usage accounting, an error naming an invalid shape or a router shim
func auxValidateLLMResponse(ctx context.Context, response any, task, provider, baseURL string) (any, error) {
	label := task
	if label == "" {
		label = "call"
	}
	if response == nil {
		return nil, &AuxRuntimeError{Message: fmt.Sprintf("Auxiliary %s: LLM returned None response", label)}
	}
	response, err := auxUnwrapDataEnvelope(response, task)
	if err != nil {
		return nil, err
	}
	if m, ok := response.(map[string]any); ok {
		RecordAuxUsage(ctx, m, task, provider, baseURL)
	}
	if !auxHasChoiceMessage(response) {
		recovered := auxRecoverAuxResponseMessage(response)
		if recovered == nil {
			text := []rune(pyrepr.Str(response))
			if len(text) > 120 {
				text = text[:120]
			}
			invalid := &AuxRuntimeError{
				Message: fmt.Sprintf("Auxiliary %s: LLM returned invalid response (type=%s): %s. Expected object with .choices[0].message — check provider "+
					"adapter or custom endpoint compatibility.", label, pyrepr.TypeName(response), pyrepr.Repr(string(text))),
				ProviderError: auxEmbeddedProviderError(response),
			}
			auxNoteRelayAuxiliaryError(ctx, invalid)
			return nil, invalid
		}
		response = recovered
	}
	if auxIsRouterTimeoutShimResponse(response) {
		shim := &AuxRuntimeError{Message: fmt.Sprintf("Auxiliary %s: provider returned a timeout shim instead of a completion", label)}
		auxNoteRelayAuxiliaryError(ctx, shim)
		return nil, shim
	}
	if record := auxRelayAuxCallContext(ctx); record != nil {
		if model, ok := auxField(response, "model", nil).(string); ok && strings.TrimSpace(model) != "" {
			record["response_model"] = model
		}
	}
	if err := auxCompleteRelayAuxiliaryCall(ctx, "", ""); err != nil {
		return nil, err
	}
	return response, nil
}

// True when the response has a non empty choices list whose first entry carries a message
func auxHasChoiceMessage(response any) bool {
	choices, ok := auxField(response, "choices", nil).([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	switch c := choices[0].(type) {
	case map[string]any:
		_, has := c["message"]
		return has
	}
	return auxField(choices[0], "message", nil) != nil
}

// True for a router failure encoded as a successful completion with no generation evidence
func auxIsRouterTimeoutShimResponse(response any) bool {
	choices, ok := auxField(response, "choices", nil).([]any)
	if !ok || len(choices) != 1 {
		return false
	}
	message := auxField(choices[0], "message", nil)
	content, ok := auxField(message, "content", nil).(string)
	if !ok || strings.TrimSpace(content) != routerTimeoutShim {
		return false
	}
	if auxTruthy(auxField(message, "tool_calls", nil)) {
		return false
	}
	usage := auxField(response, "usage", nil)
	for _, field := range []string{"completion_tokens", "output_tokens"} {
		switch v := auxField(usage, field, nil).(type) {
		case int:
			if v > 0 {
				return false
			}
		case int64:
			if v > 0 {
				return false
			}
		case float64:
			if v > 0 {
				return false
			}
		}
	}
	return true
}
