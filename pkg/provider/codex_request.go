package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

// Bounds or drops an in place prompt_cache_key entry
func boundPromptCacheKeyField(container map[string]any) {
	if container == nil {
		return
	}
	v, has := container["prompt_cache_key"]
	if !has {
		return
	}
	if bounded := boundedPromptCacheKey(v); bounded != "" {
		container["prompt_cache_key"] = bounded
	} else {
		delete(container, "prompt_cache_key")
	}
}

// Merges headers into a string coerced copy of the extra_headers entry
func mergeExtraHeaders(kwargs map[string]any, headers map[string]string) {
	merged := map[string]string{}
	switch existing := kwargs["extra_headers"].(type) {
	case map[string]string:
		for k, v := range existing {
			if k != "" {
				merged[k] = v
			}
		}
	case map[string]any:
		for k, v := range existing {
			if k != "" && v != nil {
				merged[k] = strOrEmpty(v)
			}
		}
	}
	for k, v := range headers {
		merged[k] = v
	}
	kwargs["extra_headers"] = merged
}

// Alias a client web_search goes under on xAI Responses
const xaiClientWebSearchAlias = "icebath_web_search"

var (
	opencodeReservedToolNames   = []string{"web_search", "search_files"}
	perplexityReservedToolNames = []string{"web_search", "search_files", "fetch_url", "people_search", "finance_search"}
	xaiReservedToolNames        = []string{"tool_search"}
	openAIResponsesHosts        = map[string]bool{"api.openai.com": true, "chatgpt.com": true}
)

const reservedToolAliasPrefix = "icebath_"

// Reverse alias map used only when no request built the aliases
var legacyAliasFallback = func() map[string]string {
	out := map[string]string{}
	for _, name := range append(append(append([]string{}, opencodeReservedToolNames...), perplexityReservedToolNames...), xaiReservedToolNames...) {
		out[reservedToolAliasPrefix+name] = name
	}
	out[xaiClientWebSearchAlias] = "web_search"
	return out
}()

// Parameters of one Responses request build
type codexParams struct {
	ctx                      context.Context
	instructions             string
	reasoning                reasoningCfg
	sessionID                string
	cacheScopeID             string
	maxTokens                int
	timeout                  float64
	requestOverrides         map[string]any
	provider                 string
	baseURL                  string
	isCodexBackend           bool
	isXAI                    bool
	profileReasoning         map[string]any
	replayEncryptedReasoning bool
	contextManagement        []map[string]any
	textVerbosity            string
	webSearchBackend         func() (string, error)
}

func isOpencodeResponsesBackend(p codexParams) bool {
	if OpenCodeFamily(p.provider) != "" {
		return true
	}
	return strings.ToLower(baseURLHostname(p.baseURL)) == "opencode.ai"
}

func isPerplexityResponsesBackend(p codexParams) bool {
	return strings.ToLower(baseURLHostname(p.baseURL)) == "api.perplexity.ai"
}

// Responses tool name and rename helpers for aliasReservedTools
func responsesToolName(t map[string]any) string { return stringOf(t["name"]) }

func responsesToolRename(t map[string]any, alias string) map[string]any {
	out := make(map[string]any, len(t))
	for k, v := range t {
		out[k] = v
	}
	out["name"] = alias
	return out
}

// Reports whether xAI Responses should use Grok's native web_search, failing closed to native
func xaiPrefersNativeWebSearch(p codexParams) bool {
	if p.webSearchBackend == nil {
		return true
	}
	name, err := p.webSearchBackend()
	if err != nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(name)) == "xai"
}

// Reports whether the endpoint owns the tool_search namespace
func reservesToolSearch(p codexParams) bool {
	if p.isXAI || p.isCodexBackend {
		return true
	}
	return openAIResponsesHosts[strings.ToLower(baseURLHostname(p.baseURL))]
}

// Reports whether the active web search backend selects OpenAI's server side web_search, failing closed
func openAIPrefersNativeWebSearch(p codexParams) bool {
	if p.webSearchBackend == nil {
		return false
	}
	name, err := p.webSearchBackend()
	if err != nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(name)) == "openai-native"
}

func isClientWebSearch(t map[string]any) bool { return t != nil && t["name"] == "web_search" }

// Provider reserved tool name aliasing for this request with the alias to original map
func aliasWireTools(responseTools []map[string]any, p codexParams) ([]map[string]any, map[string]string) {
	wireAliases := map[string]string{}
	hasClientWebSearch := func() bool {
		for _, t := range responseTools {
			if isClientWebSearch(t) {
				return true
			}
		}
		return false
	}
	swapNative := func() {
		var kept []map[string]any
		for _, t := range responseTools {
			if !isClientWebSearch(t) {
				kept = append(kept, t)
			}
		}
		responseTools = append(kept, map[string]any{"type": "web_search"})
	}
	if p.isXAI && len(responseTools) > 0 && hasClientWebSearch() {
		if xaiPrefersNativeWebSearch(p) {
			swapNative()
		} else {
			renamed := make([]map[string]any, 0, len(responseTools))
			for _, t := range responseTools {
				if isClientWebSearch(t) {
					renamed = append(renamed, responsesToolRename(t, xaiClientWebSearchAlias))
				} else {
					renamed = append(renamed, t)
				}
			}
			responseTools = renamed
			wireAliases[xaiClientWebSearchAlias] = "web_search"
		}
	}
	if p.isCodexBackend && len(responseTools) > 0 && hasClientWebSearch() && openAIPrefersNativeWebSearch(p) {
		swapNative()
	}
	if len(responseTools) > 0 && isOpencodeResponsesBackend(p) {
		var aliases map[string]string
		responseTools, aliases = aliasReservedTools(responseTools, opencodeReservedToolNames, responsesToolName, responsesToolRename)
		for k, v := range aliases {
			wireAliases[k] = v
		}
	}
	if len(responseTools) > 0 && isPerplexityResponsesBackend(p) {
		var aliases map[string]string
		responseTools, aliases = aliasReservedTools(responseTools, perplexityReservedToolNames, responsesToolName, responsesToolRename)
		for k, v := range aliases {
			wireAliases[k] = v
		}
	}
	if len(responseTools) > 0 && reservesToolSearch(p) {
		var aliases map[string]string
		responseTools, aliases = aliasReservedTools(responseTools, xaiReservedToolNames, responsesToolName, responsesToolRename)
		for k, v := range aliases {
			wireAliases[k] = v
		}
	}
	return responseTools, wireAliases
}

var (
	unprojectableDisableWarned   = map[string]bool{}
	unprojectableDisableWarnedMu sync.Mutex
	promptCacheOptionsDropOnce   sync.Once
)

// Effort and enabled flag for the request, clamped to the endpoint vocabulary
func resolveReasoning(model string, p codexParams) (string, bool) {
	effort, enabled := "medium", true
	if p.reasoning.set {
		if p.reasoning.disabled {
			enabled = false
		} else if p.reasoning.effort != "" {
			effort = p.reasoning.effort
		}
	}
	var supported []string
	if p.isXAI {
		if IsGrok46Family(model) {
			supported = xaiGrok46Efforts
		} else {
			supported = xaiLegacyEfforts
		}
	} else {
		var declared []string
		declaredOK := false
		if !(p.isCodexBackend || isOpenAIAPIOrigin(p.baseURL)) {
			declared, declaredOK = profileDeclaredEfforts(p.ctx, p.provider, model, p.baseURL)
		}
		if declaredOK {
			supported = declared
		} else {
			supported = codexEffortsForRoute(model, p.baseURL, p.isCodexBackend)
		}
		if len(supported) == 0 {
			return "", false
		}
	}
	if !enabled {
		hasNone := false
		for _, level := range supported {
			if strings.ToLower(strings.TrimSpace(level)) == "none" {
				hasNone = true
			}
		}
		if !hasNone {
			unprojectableDisableWarnedMu.Lock()
			if !unprojectableDisableWarned[model] {
				unprojectableDisableWarned[model] = true
				codexLog.Warn(fmt.Sprintf("reasoning_effort: none cannot be sent for %s — its route accepts only %s, so the model's default effort stays on (an omitted reasoning field does not disable it).", model, strings.Join(supported, ", ")))
			}
			unprojectableDisableWarnedMu.Unlock()
		}
		if hasNone {
			return "none", false
		}
		return "", false
	}
	return clampEffort(effort, supported, nil), enabled
}

var extendedPromptCacheModels = []string{
	"gpt-5.5-pro", "gpt-5.5", "gpt-5.4", "gpt-5.2",
	"gpt-5.1-codex-max", "gpt-5.1-codex-mini", "gpt-5.1-chat-latest", "gpt-5.1-codex", "gpt-5.1",
	"gpt-5-codex", "gpt-5", "gpt-4.1",
}

var extendedPromptCacheModelRe = func() *regexp.Regexp {
	quoted := make([]string, len(extendedPromptCacheModels))
	for i, name := range extendedPromptCacheModels {
		quoted[i] = regexp.QuoteMeta(name)
	}
	return regexp.MustCompile(`(?:^|[./:])(?:` + strings.Join(quoted, "|") + `)(?:-\d{4}-\d{2}-\d{2})?$`)
}()

// 24h cache retention for Meta and Bedrock Mantle hosts, empty elsewhere
func defaultPromptCacheRetentionForRequest(model, baseURL string) string {
	hostname := strings.ToLower(baseURLHostname(baseURL))
	if hostname == "api.meta.ai" {
		return "24h"
	}
	parts := strings.Split(hostname, ".")
	mantle := len(parts) == 4 && parts[0] == "bedrock-mantle" && parts[1] != "" && parts[2] == "api" && parts[3] == "aws"
	if !mantle {
		return ""
	}
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-")
	if extendedPromptCacheModelRe.MatchString(normalized) {
		return "24h"
	}
	return ""
}

// Exact api.openai.com host only
func isOpenAIAPIOrigin(baseURL string) bool {
	return strings.ToLower(baseURLHostname(baseURL)) == "api.openai.com"
}

// Astra on the canonical API origin only
func isOfficialOpenAIResponsesRoute(model, baseURL string) bool {
	return isAstraModel(model) && isOpenAIAPIOrigin(baseURL)
}

// Effort vocabulary of a Responses route, empty when the model takes no reasoning field
func codexEffortsForRoute(model, baseURL string, isCodexBackend bool) []string {
	if !isCodexBackend && isOpenAIAPIOrigin(baseURL) && OpenAIModelRejectsReasoning(model) {
		return nil
	}
	if isAstraModel(model) && !(isCodexBackend || isOfficialOpenAIResponsesRoute(model, baseURL)) {
		return codexLegacyEfforts
	}
	return codexSupportedEfforts(model)
}

// Astra official API contract applied after overrides
func sanitizeAstraRequestKwargs(kwargs map[string]any, model, baseURL string) {
	if !isOfficialOpenAIResponsesRoute(model, baseURL) {
		return
	}
	if reasoning, ok := kwargs["reasoning"].(map[string]any); ok {
		requested := strings.ToLower(strings.TrimSpace(strOrEmpty(reasoning["effort"])))
		if requested != "" {
			reasoning["effort"] = clampEffort(requested, codexAstraEfforts, nil)
		} else {
			reasoning["effort"] = "low"
		}
	}
	for _, key := range []string{"temperature", "top_p", "top_logprobs", "logprobs", "prompt_cache_retention"} {
		delete(kwargs, key)
	}
	if include, ok := kwargs["include"].([]any); ok {
		kept := make([]any, 0, len(include))
		for _, item := range include {
			if !strings.Contains(strings.ToLower(strOrEmpty(item)), "logprob") {
				kept = append(kept, item)
			}
		}
		kwargs["include"] = kept
	} else if include, ok := kwargs["include"].([]string); ok {
		kept := make([]any, 0, len(include))
		for _, item := range include {
			if !strings.Contains(strings.ToLower(item), "logprob") {
				kept = append(kept, item)
			}
		}
		kwargs["include"] = kept
	}
}

// Profile declared reasoning effort vocabulary by endpoint host then provider name, any hook error failing open to none
func profileDeclaredEfforts(ctx context.Context, provider, model, baseURL string) ([]string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	var (
		declared []string
		ok       bool
		err      error
	)
	if baseURL != "" {
		if inferred := inferProviderFromBaseURL(baseURL); inferred != "" && inferred != name {
			if p := getProviderProfile(ctx, inferred); p != nil {
				declared, ok, err = p.SupportedReasoningEfforts(ctx, model)
			}
		}
	}
	if err == nil && !ok && name != "" {
		if p := getProviderProfile(ctx, name); p != nil {
			declared, ok, err = p.SupportedReasoningEfforts(ctx, model)
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("profile-declared efforts lookup failed: %s", err))
		return nil, false
	}
	return declared, ok
}

// True for Microsoft Foundry's Responses API by provider id or host
func isAzureFoundryResponses(p codexParams) bool {
	if strings.ToLower(strings.TrimSpace(p.provider)) == "azure-foundry" {
		return true
	}
	return baseURLHostMatches(p.baseURL, "services.ai.azure.com")
}

// Reports whether messages end on a tool result run issued by the preceding assistant turn
func isPostToolReplay(msgs []*codexMsg) bool {
	pairIDs := func(raw string) map[string]bool {
		ids := map[string]bool{}
		embedded, itemID := splitResponsesToolID(raw)
		if embedded != "" {
			ids[embedded] = true
		}
		if len(ids) == 0 && strings.TrimSpace(raw) != "" {
			ids[strings.TrimSpace(raw)] = true
		}
		if canonical := canonicalCallIDFromFC(itemID); canonical != "" {
			ids[canonical] = true
		}
		return ids
	}
	trailing := map[string]bool{}
	for i := len(msgs) - 1; i >= 0; i-- {
		role := msgs[i].m.Role
		if role == "system" {
			continue
		}
		if role == "tool" {
			ids := pairIDs(msgs[i].m.ToolCallID)
			if len(ids) == 0 {
				return false
			}
			for id := range ids {
				trailing[id] = true
			}
			continue
		}
		if role != "assistant" {
			return false
		}
		for _, call := range msgs[i].m.ToolCalls {
			for id := range pairIDs(call.ID) {
				if trailing[id] {
					return true
				}
			}
		}
		return false
	}
	return false
}

// True for any Azure hosted Responses endpoint
func isAzureResponses(p codexParams) bool {
	if strings.ToLower(strings.TrimSpace(p.provider)) == "azure-foundry" {
		return true
	}
	return baseURLHostMatches(p.baseURL, "openai.azure.com") || baseURLHostMatches(p.baseURL, "services.ai.azure.com")
}

// Copy keeping encrypted reasoning only on the newest assistant row, checkpoints everywhere
func newestReasoningOnly(msgs []*codexMsg) []*codexMsg {
	out := make([]*codexMsg, len(msgs))
	newestKept := false
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		hasReasoning := false
		if msg.m.Role == "assistant" {
			for _, it := range msg.reasoningItems {
				if it["type"] != "compaction" {
					hasReasoning = true
				}
			}
		}
		if hasReasoning {
			if newestKept {
				var checkpoints []map[string]any
				for _, it := range msg.reasoningItems {
					if it["type"] == "compaction" {
						checkpoints = append(checkpoints, it)
					}
				}
				cp := *msg
				cp.trimmed = true
				cp.reasoningItems = checkpoints
				msg = &cp
			}
			newestKept = true
		}
		out[i] = msg
	}
	return out
}

func nativeCompactionActive(contextManagement []map[string]any) bool {
	return len(contextManagement) > 0
}

// Finite positive seconds or zero
func coerceTimeout(timeout float64) float64 {
	if timeout > 0 && timeout < 1e308 && timeout == timeout {
		return timeout
	}
	return 0
}

// Reports whether reasoning.context all_turns goes to this model on api.openai.com
func sendsAllTurns(model string, p codexParams) bool {
	return !p.isCodexBackend && isOpenAIAPIOrigin(p.baseURL) && OpenAIReasoningFamily(model) != ""
}

// reasoning and include request fields for the endpoint family
func reasoningFields(model string, p codexParams, effort string, enabled, replayEncrypted, profileReasoning, allTurns bool) map[string]any {
	include := []any{}
	if replayEncrypted {
		include = []any{"reasoning.encrypted_content"}
	}
	fields := map[string]any{}
	switch {
	case enabled && p.isXAI:
		fields["include"] = include
		if GrokSupportsReasoningEffort(model) {
			fields["reasoning"] = map[string]any{"effort": effort}
		}
	case enabled:
		if profileReasoning {
			if p.profileReasoning != nil {
				payload := make(map[string]any, len(p.profileReasoning)+1)
				for k, v := range p.profileReasoning {
					payload[k] = v
				}
				payload["summary"] = "auto"
				fields["reasoning"] = payload
			}
		} else {
			reasoning := map[string]any{"effort": effort, "summary": "auto"}
			if allTurns {
				reasoning["context"] = "all_turns"
			}
			fields["reasoning"] = reasoning
			fields["include"] = include
		}
	case !profileReasoning && !p.isXAI:
		fields["include"] = []any{}
		if effort == "none" {
			fields["reasoning"] = map[string]any{"effort": "none"}
		}
	}
	return fields
}

// Codex response status to finish reason
var codexStopReasonMap = map[string]string{"completed": "stop", "incomplete": "length", "failed": "stop", "cancelled": "stop"}

// Transport state carried across requests of one client
type codexTransport struct {
	mu               sync.Mutex
	lastIssuerKind   string
	lastIssuerModel  string
	lastWireAliases  map[string]string
	allTurnsRejected map[string]bool
	lastSentAllTurns bool
}

func newCodexTransport() *codexTransport { return &codexTransport{allTurnsRejected: map[string]bool{}} }

// RejectAllTurns omits reasoning.context for the model after a 400 naming it, true when it applied
func (t *codexTransport) RejectAllTurns(err error) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil || !t.lastSentAllTurns {
		return false
	}
	text := strings.ToLower(err.Error())
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode != 0 && apiErr.StatusCode != 400 {
		return false
	}
	if !strings.Contains(text, "reasoning.context") && !strings.Contains(text, "all_turns") {
		return false
	}
	t.allTurnsRejected[t.lastIssuerModel] = true
	t.lastSentAllTurns = false
	codexLog.Warn(fmt.Sprintf("reasoning.context rejected by %s; omitting it for this session", t.lastIssuerModel))
	return true
}

// DropUnverifiedReplay strips the replayed blobs the failing request could not vouch for from the first list
func (t *codexTransport) DropUnverifiedReplay(messageLists ...[]*state.Message) int {
	t.mu.Lock()
	kind, model := t.lastIssuerKind, t.lastIssuerModel
	t.mu.Unlock()
	if kind == "" || model == "" {
		return 0
	}
	first := 0
	for i, msgs := range messageLists {
		removed := StripUnverifiedReasoningItems(msgs, kind, model)
		if i == 0 {
			first = removed
		}
	}
	return first
}

func (t *codexTransport) resolveIssuerKind(p codexParams, endpoint *responsesEndpointQuirks) string {
	ep := endpoint
	if ep == nil {
		q := responsesEndpointQuirksFor(p.baseURL)
		ep = &q
	}
	kind := classifyResponsesIssuer(p.isXAI, p.isCodexBackend, p.baseURL, ep)
	t.mu.Lock()
	t.lastIssuerKind = kind
	t.mu.Unlock()
	return kind
}

// Chat messages converted to Responses input items
func (t *codexTransport) convertMessages(msgs []*codexMsg, p codexParams, endpoint *responsesEndpointQuirks, model string) ([]map[string]any, error) {
	t.mu.Lock()
	t.lastIssuerModel = wireModelIdentity(model)
	t.mu.Unlock()
	ep := endpoint
	if ep == nil {
		q := responsesEndpointQuirksFor(p.baseURL)
		ep = &q
	}
	return chatMessagesToResponsesInput(msgs, codexConvertOptions{
		isXAI: p.isXAI, dropReplayItemIDs: !ep.replayItemIDs, replayEncryptedReasoning: p.replayEncryptedReasoning,
		currentIssuerKind: t.resolveIssuerKind(p, ep), currentIssuerModel: wireModelIdentity(model),
		nativeCompactionEligible: nativeCompactionActive(p.contextManagement),
	})
}

// Built Responses request kwargs with the wire aliases of this request
func (t *codexTransport) buildKwargs(model string, messages []*state.Message, toolDefs []map[string]any, p codexParams) (map[string]any, error) {
	instructions := p.instructions
	payload := codexMsgsOf(messages)
	if instructions == "" && len(payload) > 0 && payload[0].m.Role == "system" {
		instructions = strings.TrimSpace(strOrEmpty(payload[0].m.Content))
		payload = payload[1:]
	}
	if instructions == "" {
		instructions = config.DefaultIdentity
	}
	endpoint := responsesEndpointQuirksFor(p.baseURL)
	replayEncrypted := p.replayEncryptedReasoning && !(isAzureFoundryResponses(p) && isPostToolReplay(payload))
	if replayEncrypted && isAzureResponses(p) {
		payload = newestReasoningOnly(payload)
	}
	p.replayEncryptedReasoning = replayEncrypted
	compactionActive := nativeCompactionActive(p.contextManagement)
	effort, enabled := resolveReasoning(model, p)
	responseTools, aliases := aliasWireTools(responsesTools(toolDefs), p)
	t.mu.Lock()
	t.lastWireAliases = aliases
	t.mu.Unlock()
	overrides := p.requestOverrides
	wireModel := model
	if m, ok := overrides["model"]; ok && strOrEmpty(m) != "" {
		wireModel = strOrEmpty(m)
	}
	wireModel = StripCodexContextVariantSuffix(wireModel)
	input, err := t.convertMessages(payload, p, &endpoint, wireModel)
	if err != nil {
		return nil, err
	}
	kwargs := map[string]any{"model": wireModel, "instructions": instructions, "input": input, "store": false}
	if len(responseTools) > 0 {
		kwargs["tools"] = responseTools
		kwargs["tool_choice"] = "auto"
		kwargs["parallel_tool_calls"] = true
	}
	if compactionActive {
		kwargs["context_management"] = p.contextManagement
	}
	scopeSource := p.cacheScopeID
	if scopeSource == "" {
		scopeSource = p.sessionID
	}
	cacheScope := cacheScopeFromSessionID(scopeSource)
	cacheKey := contentCacheKey(instructions, responseTools, cacheScope)
	if cacheKey == "" {
		cacheKey = cacheScope
	}
	if endpoint.promptCacheKey && !p.isXAI && cacheKey != "" {
		kwargs["prompt_cache_key"] = cacheKey
	}
	if retention := defaultPromptCacheRetentionForRequest(model, p.baseURL); retention != "" {
		if _, has := kwargs["prompt_cache_retention"]; !has {
			kwargs["prompt_cache_retention"] = retention
		}
	}
	t.mu.Lock()
	t.lastSentAllTurns = enabled && sendsAllTurns(wireModel, p) && !t.allTurnsRejected[wireModel]
	sentAllTurns := t.lastSentAllTurns
	t.mu.Unlock()
	for k, v := range reasoningFields(model, p, effort, enabled, replayEncrypted, endpoint.profileReasoning, sentAllTurns) {
		kwargs[k] = v
	}
	if p.textVerbosity != "" && !p.isXAI {
		kwargs["text"] = map[string]any{"verbosity": p.textVerbosity}
	}
	if len(overrides) > 0 {
		for k, v := range overrides {
			kwargs[k] = v
		}
		kwargs["model"] = wireModel
	}
	if _, has := kwargs["prompt_cache_options"]; has {
		delete(kwargs, "prompt_cache_options")
		promptCacheOptionsDropOnce.Do(func() {
			codexLog.Warn("Dropped prompt_cache_options: not a Responses.create() kwarg (use request_overrides={'extra_body': ...} for wire-only fields).")
		})
	}
	sanitizeAstraRequestKwargs(kwargs, model, p.baseURL)
	boundPromptCacheKeyField(kwargs)
	if p.isXAI {
		if !(IsGrok46Family(model) && kwargs["service_tier"] == "priority") {
			delete(kwargs, "service_tier")
		}
	}
	timeout := p.timeout
	if raw, has := kwargs["timeout"]; has {
		if f, ok := finitePositive(raw); ok {
			timeout = f
		} else {
			timeout = 0
		}
	}
	if timeout = coerceTimeout(timeout); timeout > 0 {
		kwargs["timeout"] = timeout
	} else {
		delete(kwargs, "timeout")
	}
	if p.isCodexBackend {
		headers := map[string]string{}
		if p.sessionID != "" {
			headers["session_id"] = p.sessionID
		}
		if key := strOrEmpty(kwargs["prompt_cache_key"]); key != "" {
			headers["x-client-request-id"] = key
		} else if bounded := boundedPromptCacheKey(cacheScope); bounded != "" {
			headers["x-client-request-id"] = bounded
		}
		if len(headers) > 0 {
			mergeExtraHeaders(kwargs, headers)
		}
	} else if p.maxTokens > 0 {
		kwargs["max_output_tokens"] = p.maxTokens
	}
	if p.isXAI && p.sessionID != "" {
		mergeExtraHeaders(kwargs, map[string]string{"x-grok-conv-id": cacheScope})
		extraBody := map[string]any{}
		if existing, ok := kwargs["extra_body"].(map[string]any); ok {
			for k, v := range existing {
				extraBody[k] = v
			}
		}
		if _, has := extraBody["prompt_cache_key"]; !has {
			if key, has := kwargs["prompt_cache_key"]; has {
				extraBody["prompt_cache_key"] = key
			} else {
				extraBody["prompt_cache_key"] = cacheKey
			}
		}
		kwargs["extra_body"] = extraBody
	}
	if extraBody, ok := kwargs["extra_body"].(map[string]any); ok {
		boundPromptCacheKeyField(extraBody)
	}
	return kwargs, nil
}

// Normalized Responses object as the assistant message with aliases reversed and the finish reason
func (t *codexTransport) normalizeResponse(resp map[string]any, issuerKind, issuerModel string) (*state.Message, string, error) {
	t.mu.Lock()
	if issuerKind == "" {
		issuerKind = t.lastIssuerKind
	}
	if issuerModel == "" {
		issuerModel = t.lastIssuerModel
	}
	aliasMap := t.lastWireAliases
	t.mu.Unlock()
	msg, finish, err := normalizeCodexResponse(resp, issuerKind, issuerModel, true)
	if err != nil {
		return nil, "", err
	}
	out := &state.Message{Role: "assistant", Content: msg.Content, FinishReason: finish}
	if msg.Reasoning != "" {
		out.Reasoning = msg.Reasoning
	}
	for _, tc := range msg.ToolCalls {
		name := tc.Name
		if aliasMap == nil {
			if original, ok := legacyAliasFallback[name]; ok {
				name = original
			}
		} else if original, ok := aliasMap[name]; ok {
			name = original
		}
		id := tc.ID
		if id == "" {
			id = name
		}
		out.ToolCalls = append(out.ToolCalls, state.ToolCall{ID: id, Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: tc.Arguments}})
	}
	out.CodexReasoningItems = encodeCodexItems(msg.ReasoningItems)
	out.CodexMessageItems = encodeCodexItems(msg.MessageItems)
	if finish == "" {
		finish = "stop"
	}
	return out, finish, nil
}

// Validated and sanitized request kwargs before the call
func (t *codexTransport) preflightKwargs(apiKwargs map[string]any, allowStream, dropReplayItemIDs, sanitizeHarmony bool) (map[string]any, error) {
	normalized, err := preflightCodexAPIKwargs(apiKwargs, allowStream, dropReplayItemIDs, sanitizeHarmony)
	if err != nil {
		return nil, err
	}
	boundPromptCacheKeyField(normalized)
	if extraBody, ok := normalized["extra_body"].(map[string]any); ok {
		boundPromptCacheKeyField(extraBody)
	}
	return normalized, nil
}
