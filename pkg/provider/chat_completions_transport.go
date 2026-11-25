package provider

import (
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var ccStripMsgKeys = map[string]bool{
	"codex_reasoning_items": true, "codex_message_items": true, "tool_name": true, "effect_disposition": true, "timestamp": true,
	"platform_message_id": true, "message_id": true, "api_content": true, "anthropic_content_blocks": true,
	"bedrock_content_blocks": true, "message_uid": true,
}

var ccStripTCKeys = []string{"call_id", "response_item_id"}

var ccReasoningCarrierKeys = []string{"reasoning_content", "reasoning", "reasoning_details"}

// Provider profile a transport kwarg carries, nil when absent
func transportProfile(v any) *Profile {
	switch p := v.(type) {
	case *Profile:
		return p
	case Profile:
		return &p
	}
	return nil
}

// True when the exactly named canonical provider profile replays tool call signatures
func profileReplaysToolCallSignatures(provider string) bool {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return false
	}
	profile, ok := GetProviderProfile(nil, name)
	return ok && profile.Name == name && profile.ReplaysToolCallSignatures
}

// True when the Gemini sidecar holds a non blank string thought signature at the top or under google
func hasReplayableThoughtSignature(extra any) bool {
	m, ok := extra.(map[string]any)
	if !ok {
		return false
	}
	candidate, present := m["thought_signature"]
	if !present || candidate == nil {
		if google, ok := m["google"].(map[string]any); ok {
			candidate = google["thought_signature"]
		}
	}
	s, ok := candidate.(string)
	return ok && strings.TrimSpace(s) != ""
}

// List view of a tool_calls value, false for anything that is not a list
func anyList(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// Copy of a message without persistence sidecars, scaffolding markers and unread carriers, nil when nothing changes
func sanitizeChatMessage(msg map[string]any, stripExtra bool, stripReasoning []string, nativeType string, mistralIDs bool) map[string]any {
	if msg == nil {
		return nil
	}
	strip := map[string]bool{}
	for k := range msg {
		if ccStripMsgKeys[k] || strings.HasPrefix(k, "_") {
			strip[k] = true
		}
	}
	role := msg["role"]
	if role == "assistant" {
		for _, k := range stripReasoning {
			if _, ok := msg[k]; ok {
				strip[k] = true
			}
		}
	}
	var keptDetails []any
	if details, ok := msg["reasoning_details"].([]any); ok && !strip["reasoning_details"] {
		kept := []any{}
		for _, d := range details {
			if dm, ok := jsonutil.AsMap(d); ok {
				if typ, ok := dm["type"].(string); ok && strings.HasSuffix(typ, ".native_assistant") && typ != nativeType {
					continue
				}
			}
			kept = append(kept, d)
		}
		if len(kept) != len(details) {
			strip["reasoning_details"] = true
			keptDetails = kept
		}
	}
	if _, ok := msg["name"]; ok && role == "tool" {
		strip["name"] = true
	}
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		if !strip[k] {
			out[k] = v
		}
	}
	remapped := false
	if mistralIDs && role == "tool" {
		if key := state.CoalesceToolCallID(map[string]any{"id": msg["tool_call_id"]}); key != "" {
			if mapped := mistralToolID(key); msg["tool_call_id"] != any(mapped) {
				out["tool_call_id"] = mapped
				remapped = true
			}
		}
	}
	if len(keptDetails) > 0 {
		out["reasoning_details"] = keptDetails
	}
	raw, hasTC := msg["tool_calls"]
	list, isList := anyList(raw)
	var copied []any
	if role == "assistant" && hasTC && (raw == nil || (isList && len(list) == 0)) {
		delete(out, "tool_calls")
		strip["tool_calls"] = true
	} else if isList {
		for idx, tc := range list {
			tco, ok := jsonutil.AsObject(tc)
			if !ok {
				continue
			}
			tcm := tco.Map()
			drop := map[string]bool{}
			for _, k := range ccStripTCKeys {
				if _, ok := tcm[k]; ok {
					drop[k] = true
				}
			}
			if ec, ok := tcm["extra_content"]; ok && (stripExtra || !hasReplayableThoughtSignature(ec)) {
				drop["extra_content"] = true
			}
			key := ""
			if mistralIDs {
				key = state.CoalesceToolCallID(tcm)
			}
			mapped := ""
			if key != "" {
				mapped = mistralToolID(key)
			}
			if len(drop) == 0 && (key == "" || tcm["id"] == any(mapped)) {
				continue
			}
			if copied == nil {
				copied = append([]any(nil), list...)
			}
			next := jsonutil.NewObject()
			for _, k := range tco.Keys() {
				if !drop[k] {
					next.Set(k, tcm[k])
				}
			}
			if key != "" {
				next.Set("id", mapped)
			}
			if _, plain := tc.(map[string]any); plain {
				copied[idx] = next.Map()
			} else {
				copied[idx] = next
			}
		}
		if copied != nil {
			if _, typed := raw.([]map[string]any); typed {
				maps := make([]map[string]any, len(copied))
				for i, c := range copied {
					maps[i], _ = c.(map[string]any)
				}
				out["tool_calls"] = maps
			} else {
				out["tool_calls"] = copied
			}
		}
	}
	if len(strip) > 0 || remapped || copied != nil {
		return out
	}
	return nil
}

// Wire copy of the messages with fields strict chat completions providers reject removed, the input list when clean
func chatCompletionsSanitizeMessages(messages []map[string]any, kwargs map[string]any) any {
	profile := transportProfile(kwargs["provider_profile"])
	provider := kwargString(kwargs, "provider")
	nativeType := ""
	if profile != nil {
		if profile.Name != "" {
			provider = profile.Name
		}
		nativeType = profile.NativeReasoningDetailsType
	}
	model, baseURL := kwargString(kwargs, "model"), kwargString(kwargs, "base_url")
	stripExtra := !modelConsumesThoughtSignature(model, baseURL, provider)
	carriers := routeReasoningCarriers(provider, baseURL)
	if nativeType != "" && len(carriers) > 0 {
		carriers["reasoning_details"] = true
	}
	var stripReasoning []string
	for _, k := range ccReasoningCarrierKeys {
		if !carriers[k] {
			stripReasoning = append(stripReasoning, k)
		}
	}
	mistralIDs := routeRequiresMistralToolIDs(provider, model, baseURL)
	var out []map[string]any
	for i, m := range messages {
		s := sanitizeChatMessage(m, stripExtra, stripReasoning, nativeType, mistralIDs)
		if s == nil {
			continue
		}
		if out == nil {
			out = append([]map[string]any(nil), messages...)
		}
		out[i] = s
	}
	if out == nil {
		return messages
	}
	return out
}

// Transport convert_messages over message maps with only the model and base URL kwargs
func chatCompletionsConvertMessages(messages []map[string]any, model, baseURL string) []map[string]any {
	out, _ := chatCompletionsSanitizeMessages(messages, map[string]any{"model": model, "base_url": baseURL}).([]map[string]any)
	return out
}

// Reasoning config dict of a kwarg, nil for anything else
func reasoningConfigDict(v any) map[string]any {
	switch r := v.(type) {
	case map[string]any:
		return r
	case *Reasoning:
		return reasoningConfigMap(r)
	case Reasoning:
		return reasoningConfigMap(&r)
	}
	return nil
}

// Ordered config with its effort clamped onto the supported wire vocabulary, the same dict when unchanged
func clampReasoningConfigObject(rc *jsonutil.Object, supported []string) *jsonutil.Object {
	if rc == nil {
		return nil
	}
	effort := ""
	if anyutil.IsTrue(rc.Value("effort")) {
		effort = strings.ToLower(strings.TrimSpace(anyutil.String(rc.Value("effort"))))
	}
	clamped := effort
	if effort != "" {
		clamped = clampEffort(effort, supported, nil)
	}
	if clamped == effort {
		return rc
	}
	out := rc.Clone()
	out.Set("effort", clamped)
	return out
}

// Config with its effort clamped onto the supported wire vocabulary, the same dict when unchanged
func clampReasoningConfigDict(rc map[string]any, supported []string) map[string]any {
	if rc == nil {
		return nil
	}
	effort := ""
	if anyutil.IsTrue(rc["effort"]) {
		effort = strings.ToLower(strings.TrimSpace(anyutil.String(rc["effort"])))
	}
	clamped := effort
	if effort != "" {
		clamped = clampEffort(effort, supported, nil)
	}
	if clamped == effort {
		return rc
	}
	out := copyDict(rc)
	out["effort"] = clamped
	return out
}

// Explicit lowercase effort of a config dict, empty when absent or reasoning is disabled
func requestedEffortDict(rc map[string]any) string {
	if rc == nil {
		return ""
	}
	if enabled, ok := rc["enabled"].(bool); ok && !enabled {
		return ""
	}
	if !anyutil.IsTrue(rc["effort"]) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(anyutil.String(rc["effort"])))
}

// Hook reasoning value of a config dict, nil when the config is unset
func reasoningPtrOfDict(rc map[string]any) *Reasoning {
	if rc == nil {
		return nil
	}
	r := reasoningFromConfig(rc)
	if e, ok := rc["effort"]; ok && e != nil {
		if _, isStr := e.(string); !isStr {
			r.Effort = anyutil.String(e)
		}
	}
	return &r
}

// ReasoningOfConfig is the hook reasoning value of a reasoning config dict, nil when the config is unset
func ReasoningOfConfig(rc map[string]any) *Reasoning { return reasoningPtrOfDict(rc) }

// Output cap raised to the Gemini published ceiling when thinking would exhaust it, unchanged otherwise
func raiseGeminiThinkingMaxTokensAny(model string, rc map[string]any, requested any) any {
	cfg := buildGeminiThinkingConfig(model, reasoningFromRequest(reasoningFromConfig(rc)))
	if len(cfg) == 0 {
		return requested
	}
	return effectiveGeminiMaxOutputTokens(requested, cfg)
}

// Integer form of a numeric kwarg value
func kwargNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

// Callable of the max_tokens_param_fn kwarg mapping a cap to its wire fields, nil when absent
func maxTokensParamFn(v any) func(any) map[string]any {
	switch fn := v.(type) {
	case func(any) map[string]any:
		return fn
	case func(int) map[string]any:
		return func(n any) map[string]any { return fn(kwargNumber(n)) }
	}
	return nil
}

// Writes the output cap from the task budget, the caller cap, then the profile default
func applyMaxTokens(api map[string]any, model string, rc map[string]any, params map[string]any, profileMax int) {
	fn := maxTokensParamFn(params["max_tokens_param_fn"])
	for _, key := range []string{"ephemeral_max_output_tokens", "max_tokens"} {
		if c := params[key]; c != nil && fn != nil {
			for k, v := range fn(raiseGeminiThinkingMaxTokensAny(model, rc, c)) {
				api[k] = v
			}
			return
		}
	}
	if profileMax != 0 && fn != nil {
		for k, v := range fn(raiseGeminiThinkingMaxTokensAny(model, rc, profileMax)) {
			api[k] = v
		}
	}
}

// Messages with a leading system role renamed developer for GPT-5 and Codex models
func swapDeveloperRole(sanitized []map[string]any, modelLower string) []map[string]any {
	if len(sanitized) == 0 || sanitized[0] == nil || sanitized[0]["role"] != "system" {
		return sanitized
	}
	for _, p := range DeveloperRoleModels {
		if strings.Contains(modelLower, p) {
			first := copyDict(sanitized[0])
			first["role"] = "developer"
			return append([]map[string]any{first}, sanitized[1:]...)
		}
	}
	return sanitized
}

// Model, messages, temperature, timeout and tools scaffold shared by both build paths
func baseKwargs(model string, sanitized []map[string]any, tools []map[string]any, params map[string]any, profile *Profile) map[string]any {
	api := map[string]any{"model": model, "messages": sanitized}
	if profile != nil {
		fixed, omit := profile.TemperatureOverride()
		switch {
		case omit:
		case fixed != nil:
			api["temperature"] = *fixed
		case params["temperature"] != nil:
			api["temperature"] = params["temperature"]
		}
	}
	if params["timeout"] != nil {
		api["timeout"] = params["timeout"]
	}
	if len(tools) > 0 {
		if isMoonshotModel(model) {
			api["tools"] = sanitizeMoonshotTools(tools)
		} else {
			api["tools"] = tools
		}
	}
	return api
}

// Leading system or developer content used for cache routing, JSON with sorted keys for structured content
func staticPromptInstructionsDicts(messages []map[string]any) string {
	if len(messages) == 0 || messages[0] == nil {
		return ""
	}
	first := messages[0]
	if first["role"] != "system" && first["role"] != "developer" {
		return ""
	}
	content := first["content"]
	if s, ok := content.(string); ok {
		return s
	}
	text, err := jsonutil.Encode(content, jsonutil.SortKeys())
	if err != nil {
		if !anyutil.IsTrue(content) {
			return ""
		}
		return anyutil.String(content)
	}
	return text
}

// Bounds a caller prompt_cache_key in place or adds the content addressed key for a capable endpoint
func addPromptCacheKey(api map[string]any, messages []map[string]any, supports bool, sessionID, cacheScopeID string) {
	found := false
	if _, has := api["prompt_cache_key"]; has {
		boundPromptCacheKeyField(api)
		found = true
	}
	if extra, ok := api["extra_body"].(*jsonutil.Object); ok {
		if v, has := extra.Get("prompt_cache_key"); has {
			if bounded := boundedPromptCacheKey(v); bounded != "" {
				extra.Set("prompt_cache_key", bounded)
			} else {
				extra.Delete("prompt_cache_key")
			}
			found = true
		}
	}
	if found || !supports {
		return
	}
	tools, _ := api["tools"].([]map[string]any)
	scope := cacheScopeID
	if scope == "" {
		scope = sessionID
	}
	if key := contentCacheKey(staticPromptInstructionsDicts(messages), tools, cacheScopeFromSessionID(scope)); key != "" {
		api["prompt_cache_key"] = key
	}
}

// True only for the exact api.openai.com host
func isOpenAIAPIBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.ToLower(u.Hostname()) == "api.openai.com"
}

// Pareto router coding score floor within zero and one, false when absent or out of range
func paretoScore(raw any) (float64, bool) {
	var score float64
	switch v := raw.(type) {
	case nil:
		return 0, false
	case bool:
		if v {
			score = 1
		}
	case int:
		score = float64(v)
	case int64:
		score = float64(v)
	case float64:
		score = v
	case string:
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), "_", ""), 64)
		if err != nil {
			return 0, false
		}
		score = f
	default:
		return 0, false
	}
	if math.IsNaN(score) || score < 0 || score > 1 {
		return 0, false
	}
	return score, true
}

// Merges every entry of src into dst
func mergeInto(dst map[string]any, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// Chat completions request kwargs from a provider profile or, for unregistered providers, the legacy route flags
func chatCompletionsBuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error) {
	params = copyDict(params)
	profile := transportProfile(params["provider_profile"])

	sanitized := chatCompletionsSanitizeMessages(messages, map[string]any{
		"model": model, "base_url": params["base_url"], "provider_profile": params["provider_profile"], "provider": params["provider_name"],
	}).([]map[string]any)
	if profile != nil {
		return buildKwargsFromProfile(profile, model, sanitized, tools, params), nil
	}
	modelLower := strings.ToLower(model)
	if v, ok := params["model_lower"]; ok {
		modelLower, _ = v.(string)
	}
	sanitized = swapDeveloperRole(sanitized, modelLower)
	api := baseKwargs(model, sanitized, tools, params, nil)
	isKimi := kwargBool(params, "is_kimi", false)
	rc := clampReasoningConfigDict(reasoningConfigDict(params["reasoning_config"]), openAICompatWireEfforts)
	applyMaxTokens(api, model, rc, params, 0)
	enabled, hasEnabled := rc["enabled"].(bool)
	thinkingOff := hasEnabled && !enabled
	requested := requestedEffortDict(rc)
	if isKimi && !thinkingOff {
		supported := kimiSupportedEfforts(model)
		isK3 := len(supported) > 0 && &supported[0] == &kimiK3Efforts[0]
		switch {
		case requested == "" && isK3:
			api["reasoning_effort"] = "high"
		case requested == "":
			api["reasoning_effort"] = "medium"
		case isK3:
			api["reasoning_effort"] = clampEffort(requested, supported, kimiK3Overrides)
		default:
			api["reasoning_effort"] = clampEffort(requested, supported, nil)
		}
	}
	if kwargBool(params, "is_tokenhub", false) && !thinkingOff {
		api["reasoning_effort"] = tokenhubEffort(requested)
	}
	extraBody := jsonutil.NewObject()
	isOpenRouter := kwargBool(params, "is_openrouter", false)
	baseURL := kwargString(params, "base_url")
	if isOpenRouter && anyutil.IsTrue(params["provider_preferences"]) {
		extraBody.Set("provider", params["provider_preferences"])
	}
	if isOpenRouter && model == "openrouter/pareto-code" {
		if score, ok := paretoScore(params["openrouter_min_coding_score"]); ok {
			extraBody.Set("plugins", []any{jsonutil.ObjectOf("id", "pareto-router", "min_coding_score", score)})
		}
	}
	if isKimi {
		toggle := "enabled"
		if thinkingOff {
			toggle = "disabled"
		}
		extraBody.Set("thinking", jsonutil.ObjectOf("type", toggle))
	}
	if kwargBool(params, "supports_reasoning", false) {
		if kwargBool(params, "profile_endpoint", false) {
			if params["endpoint_reasoning"] != nil {
				extraBody.Set("reasoning", params["endpoint_reasoning"])
			}
		} else {
			var effort any = "medium"
			if len(rc) > 0 {
				if e, ok := rc["effort"]; ok && anyutil.IsTrue(e) {
					effort = e
				}
			}
			off := thinkingOff || effort == any("none")
			if off {
				extraBody.Set("reasoning", jsonutil.ObjectOf("enabled", false, "effort", "none"))
			} else {
				extraBody.Set("reasoning", jsonutil.ObjectOf("enabled", true, "effort", effort))
			}
		}
	}
	if strings.ToLower(strings.TrimSpace(anyutil.String(params["provider_name"]))) == "gemini" && params["provider_name"] != nil {
		raw := buildGeminiThinkingConfig(model, reasoningFromRequest(reasoningFromConfig(rc)))
		if geminiOpenAICompatBaseURL(baseURL) {
			if tc := snakeCaseGeminiThinkingConfig(raw); len(tc) > 0 {
				compat, _ := jsonutil.AsObject(extraBody.Value("extra_body"))
				if compat == nil {
					compat = jsonutil.NewObject()
				}
				google := jsonutil.NewObject()
				if prev, ok := jsonutil.AsObject(compat.Value("google")); ok {
					google = prev.Clone()
				}
				google.Set("thinking_config", jsonutil.Ordered(tc))
				compat.Set("google", google)
				extraBody.Set("extra_body", compat)
			}
		} else if len(raw) > 0 {
			extraBody.Set("thinking_config", jsonutil.Ordered(raw))
		}
	}
	if additions, ok := jsonutil.AsObject(params["extra_body_additions"]); ok && additions.Len() > 0 {
		extraBody.Merge(additions)
	}
	if extraBody.Len() > 0 {
		api["extra_body"] = extraBody
	}
	if overrides, ok := params["request_overrides"].(map[string]any); ok && len(overrides) > 0 {
		mergeInto(api, overrides)
	}
	supports := kwargBool(params, "supports_prompt_cache_key", false) || isOpenAIAPIBaseURL(baseURL)
	addPromptCacheKey(api, sanitized, supports, kwargString(params, "session_id"), kwargString(params, "cache_scope_id"))
	return api, nil
}

// Request kwargs where every provider quirk comes from the profile hooks
func buildKwargsFromProfile(profile *Profile, model string, sanitized []map[string]any, tools []map[string]any, params map[string]any) map[string]any {
	sanitized = swapDeveloperRole(profile.PrepareMessages(sanitized), strings.ToLower(model))
	api := baseKwargs(model, sanitized, tools, params, profile)
	rc := clampReasoningConfigDict(reasoningConfigDict(params["reasoning_config"]), openAICompatWireEfforts)
	applyMaxTokens(api, model, rc, params, profile.GetMaxTokens(model))
	reasoning := reasoningPtrOfDict(rc)
	extrasCtx := map[string]any{
		"reasoning_config": reasoning, "supports_reasoning": kwargBool(params, "supports_reasoning", false),
		"qwen_session_metadata": params["qwen_session_metadata"], "model": model, "base_url": params["base_url"],
		"ollama_num_ctx": params["ollama_num_ctx"], "session_id": params["session_id"], "cache_scope_id": params["cache_scope_id"],
	}
	if lm := params["lmstudio_reasoning_options"]; lm != nil {
		extrasCtx["lmstudio_reasoning_options"] = lm
	}
	bodyCtx := map[string]any{
		"session_id": params["session_id"], "provider_preferences": params["provider_preferences"], "model": model,
		"base_url": params["base_url"], "reasoning_config": reasoning,
		"openrouter_min_coding_score": params["openrouter_min_coding_score"],
	}
	if c, ok := params[hookCtxKey]; ok {
		extrasCtx[hookCtxKey] = c
		bodyCtx[hookCtxKey] = c
	}
	extraFromProfile, top := profile.BuildAPIKwargsExtras(reasoning, extrasCtx)
	mergeInto(api, top)
	extraBody := jsonutil.NewObject()
	profileBody := profile.BuildExtraBody(kwargString(params, "session_id"), bodyCtx)
	additions, _ := jsonutil.AsObject(params["extra_body_additions"])
	for _, part := range []*jsonutil.Object{profileBody, extraFromProfile, additions} {
		extraBody.Merge(part)
	}
	if overrides, ok := jsonutil.AsObject(params["request_overrides"]); ok {
		for _, k := range overrides.Keys() {
			v := overrides.Value(k)
			if sub, isDict := jsonutil.AsObject(v); k == "extra_body" && isDict {
				extraBody.Merge(sub)
			} else {
				api[k] = v
			}
		}
	}
	if extraBody.Len() > 0 {
		if isNativeGeminiBaseURL(kwargString(params, "base_url")) {
			kept := jsonutil.NewObject()
			for _, k := range extraBody.Keys() {
				if k == "thinking_config" || k == "thinkingConfig" || k == "max_output_tokens" {
					kept.Set(k, extraBody.Value(k))
				}
			}
			extraBody = kept
		}
		if extraBody.Len() > 0 {
			api["extra_body"] = extraBody
		}
	}
	addPromptCacheKey(api, sanitized, profile.SupportsPromptCacheKey, kwargString(params, "session_id"), kwargString(params, "cache_scope_id"))
	return api
}
