package provider

import (
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

var ccStripMsgKeys = map[string]bool{
	"codex_reasoning_items": true, "codex_message_items": true, "tool_name": true, "effect_disposition": true, "timestamp": true,
	"platform_message_id": true, "message_id": true, "api_content": true, "anthropic_content_blocks": true,
	"bedrock_content_blocks": true, "message_uid": true, "data_use": true,
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
			if dm, ok := d.(map[string]any); ok {
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
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
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
			next := make(map[string]any, len(tcm))
			for k, v := range tcm {
				if !drop[k] {
					next[k] = v
				}
			}
			if key != "" {
				next["id"] = mapped
			}
			copied[idx] = next
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

// Union of the data_use requirements the messages carry
func dataUseOfDicts(messages []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, m := range messages {
		switch v := m["data_use"].(type) {
		case []any:
			for _, r := range v {
				if s, ok := r.(string); ok {
					out[s] = true
				}
			}
		case []string:
			for _, s := range v {
				out[s] = true
			}
		case map[string]bool:
			for s, on := range v {
				if on {
					out[s] = true
				}
			}
		}
	}
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

// Config with its effort clamped onto the OpenAI compatible wire vocabulary, the same dict when unchanged
func clampReasoningConfigDict(rc map[string]any) map[string]any {
	if rc == nil {
		return nil
	}
	effort := ""
	if pyTruthy(rc["effort"]) {
		effort = strings.ToLower(strings.TrimSpace(pyStr(rc["effort"])))
	}
	clamped := effort
	if effort != "" {
		clamped = clampEffort(effort, openAICompatWireEfforts, nil)
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
	if !pyTruthy(rc["effort"]) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(pyStr(rc["effort"])))
}

// Hook reasoning value of a config dict, nil when the config is unset
func reasoningPtrOfDict(rc map[string]any) *Reasoning {
	if rc == nil {
		return nil
	}
	r := reasoningFromConfig(rc)
	if e, ok := rc["effort"]; ok && e != nil {
		if _, isStr := e.(string); !isStr {
			r.Effort = pyStr(e)
		}
	}
	return &r
}

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
	text, err := pyjson.Dumps(content, pyjson.SortKeys(true), pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		if !pyTruthy(content) {
			return ""
		}
		return pyStr(content)
	}
	return text
}

// Bounds a caller prompt_cache_key in place or adds the content addressed key for a capable endpoint
func addPromptCacheKey(api map[string]any, messages []map[string]any, supports bool, sessionID, cacheScopeID string) {
	found := false
	for _, c := range []any{api, api["extra_body"]} {
		if m, ok := c.(map[string]any); ok {
			if _, has := m["prompt_cache_key"]; has {
				boundPromptCacheKeyField(m)
				found = true
			}
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
	params["data_use"] = dataUseOfDicts(messages)
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
	rc := clampReasoningConfigDict(reasoningConfigDict(params["reasoning_config"]))
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
	extraBody := map[string]any{}
	isOpenRouter := kwargBool(params, "is_openrouter", false)
	baseURL := kwargString(params, "base_url")
	if isOpenRouter && pyTruthy(params["provider_preferences"]) {
		extraBody["provider"] = params["provider_preferences"]
	}
	if isOpenRouter && model == "openrouter/pareto-code" {
		if score, ok := paretoScore(params["openrouter_min_coding_score"]); ok {
			extraBody["plugins"] = []any{map[string]any{"id": "pareto-router", "min_coding_score": score}}
		}
	}
	if isKimi {
		toggle := "enabled"
		if thinkingOff {
			toggle = "disabled"
		}
		extraBody["thinking"] = map[string]any{"type": toggle}
	}
	if kwargBool(params, "supports_reasoning", false) {
		if kwargBool(params, "profile_endpoint", false) {
			if params["endpoint_reasoning"] != nil {
				extraBody["reasoning"] = params["endpoint_reasoning"]
			}
		} else {
			var effort any = "medium"
			if len(rc) > 0 {
				if e, ok := rc["effort"]; ok && pyTruthy(e) {
					effort = e
				}
			}
			off := thinkingOff || effort == any("none")
			if off {
				extraBody["reasoning"] = map[string]any{"enabled": false, "effort": "none"}
			} else {
				extraBody["reasoning"] = map[string]any{"enabled": true, "effort": effort}
			}
		}
	}
	if strings.ToLower(strings.TrimSpace(pyStr(params["provider_name"]))) == "gemini" && params["provider_name"] != nil {
		raw := buildGeminiThinkingConfig(model, reasoningFromRequest(reasoningFromConfig(rc)))
		if geminiOpenAICompatBaseURL(baseURL) {
			if tc := snakeCaseGeminiThinkingConfig(raw); len(tc) > 0 {
				compat, _ := extraBody["extra_body"].(map[string]any)
				if compat == nil {
					compat = map[string]any{}
				}
				google, _ := compat["google"].(map[string]any)
				google = copyDict(google)
				google["thinking_config"] = tc
				compat["google"] = google
				extraBody["extra_body"] = compat
			}
		} else if len(raw) > 0 {
			extraBody["thinking_config"] = raw
		}
	}
	if additions, ok := params["extra_body_additions"].(map[string]any); ok && len(additions) > 0 {
		mergeInto(extraBody, additions)
	}
	if len(extraBody) > 0 {
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
	rc := clampReasoningConfigDict(reasoningConfigDict(params["reasoning_config"]))
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
		"openrouter_min_coding_score": params["openrouter_min_coding_score"], "data_use": params["data_use"],
	}
	if c, ok := params[hookCtxKey]; ok {
		extrasCtx[hookCtxKey] = c
		bodyCtx[hookCtxKey] = c
	}
	extraFromProfile, top := profile.BuildAPIKwargsExtras(reasoning, extrasCtx)
	mergeInto(api, top)
	extraBody := map[string]any{}
	profileBody := profile.BuildExtraBody(kwargString(params, "session_id"), bodyCtx)
	additions, _ := params["extra_body_additions"].(map[string]any)
	for _, part := range []map[string]any{profileBody, extraFromProfile, additions} {
		mergeInto(extraBody, part)
	}
	if overrides, ok := params["request_overrides"].(map[string]any); ok {
		for k, v := range overrides {
			if sub, isDict := v.(map[string]any); k == "extra_body" && isDict {
				mergeInto(extraBody, sub)
			} else {
				api[k] = v
			}
		}
	}
	if len(extraBody) > 0 {
		if isNativeGeminiBaseURL(kwargString(params, "base_url")) {
			kept := map[string]any{}
			for _, k := range []string{"thinking_config", "thinkingConfig", "max_output_tokens"} {
				if v, ok := extraBody[k]; ok {
					kept[k] = v
				}
			}
			extraBody = kept
		}
		if len(extraBody) > 0 {
			api["extra_body"] = extraBody
		}
	}
	addPromptCacheKey(api, sanitized, profile.SupportsPromptCacheKey, kwargString(params, "session_id"), kwargString(params, "cache_scope_id"))
	return api
}
