package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func moaLoopLog() *slog.Logger { return slog.Default().With("logger", "agent.moa_loop") }

var (
	moaEmailRE = mustPyRE(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	moaPhoneRE = mustPyRE(`(?<![\w.+-])(?:\+?1[ .-])?(?:\(\d{3}\)[ .-]?|\d{3}[.-])\d{3}[.-]\d{4}(?![\w-])`)
)

// Seams over the provider, config and clock calls the fan out makes, swapped by tests
var (
	moaCallLLM     = provider.CallLLM
	moaLoadConfig  = func(ctx context.Context) (map[string]any, error) { return loadMoAFullConfig(ctx) }
	moaNow         = time.Now
	moaResolveSlot = func(ctx context.Context, requested, model string) (*provider.Runtime, error) {
		return provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), requested, model, "", "")
	}
	moaGetModelContextLength = func(ctx context.Context, req provider.ContextLengthRequest) (int, error) {
		return provider.GetModelContextLength(ctx, req), nil
	}
	moaEstimateMessagesTokensRough = func(ctx context.Context, messages []map[string]any) int {
		imageCost := provider.CurrentImageTokenCost(ctx)
		total := 0
		for _, m := range messages {
			total += provider.EstimateMessageTokensRough(m, imageCost)
		}
		return total
	}
	moaRuntimeCacheTTL          = 300 * time.Second
	moaReferencePollInterval    = 5 * time.Second
	moaReferenceInterruptSettle = 50 * time.Millisecond
)

// Whole profile config as load_config returns it, the safe worker snapshot included
func loadMoAFullConfig(ctx context.Context) (map[string]any, error) {
	cfg, err := config.Load(home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return map[string]any(cfg), nil
}

const (
	moaMaxReferenceWorkers            = 8
	moaReferenceToolResultBudget      = 4000
	moaReferenceDefaultOutputReserve  = 8192
	moaReferenceTrimSafetyFraction    = 0.10
	moaInterruptedReferenceNote       = "[skipped: interrupted by user]"
	moaRecursiveReferenceNote         = "[skipped: MoA presets cannot recursively reference MoA]"
	moaVirtualAPIKey                  = "moa-virtual-provider"
	moaVirtualBaseURL                 = "moa://local"
	moaReferenceSystemPrompt          = "You are a reference advisor in a Mixture of Agents (MoA) process. You are " + "NOT the acting agent and you do NOT execute anything: you cannot call " + "tools, run commands, browse, or access files, repositories, or URLs, and " + "you should not try to or apologize for being unable to. A separate " + "aggregator/orchestrator model holds those capabilities and will take the " + "actual actions.\n\n" + "CRITICAL: You must NEVER claim or imply that you have executed a command, " + "downloaded a file, accessed a URL, or performed any action. You can only " + "analyze and advise based on the conversation context. Examples of what to " + "avoid:\n" + "- Bad: \"I ran curl and got 404.\"\n" + "- Bad: \"I downloaded the file successfully.\"\n" + "- Bad: \"I checked the repository and found...\"\n" + "- Good: \"Based on the error pattern, a curl request to that URL would likely return 404.\"\n" + "- Good: \"The conversation suggests downloading this file may help.\"\n" + "- Good: \"From the context, checking the repository would reveal...\"\n\n" + "The conversation below is the current state of a task handled by that " + "acting agent. Your job is to give your most intelligent analysis of that " + "state: understand the goal, reason about the problem, and advise on what " + "to do next. Surface the best approach, concrete next steps and tool-use " + "strategy, likely pitfalls and risks, and anything the acting agent may " + "have missed or gotten wrong. Assume any referenced files, URLs, or " + "systems exist and reason about them from the context given rather than " + "asking for access.\n\n" + "Respond with your advice directly — no preamble, no disclaimers about " + "tools or access. Advise in prose: never emit a tool call or a JSON " + "tool-call object, because the aggregator replays what looks like one. " + "Your response is private guidance handed to the " + "aggregator, not an answer shown to the user. NEVER claim to have executed " + "anything."
	moaStaleGuidanceNote              = "This guidance was produced earlier in this turn, before the tool results below it. " + "Check the transcript before acting on it: a step it suggests may already have run, and " + "repeating a completed tool call is never the next step.\n"
	moaAdvisoryInstruction            = "[The conversation above is the current state of the task. Give your " + "most intelligent judgement: what is going on, what should happen next, " + "what risks or mistakes you see, and how the acting agent should " + "proceed.]"
	moaImageOnlyUserNote              = "[user sent non-text content (e.g. an image attachment)]"
	moaAllFailedAggregateHeader       = "[Mixture of Agents context — all reference models failed. " + "Proceeding without aggregated guidance.]\n"
	moaAggregateContextHeader         = "[Mixture of Agents context — use this as private guidance for the " + "normal Hermes agent loop. You may call tools, continue reasoning, or " + "finish normally.]\n"
	moaAggregatorSynthesisInstruction = "You are the aggregator in a Mixture of Agents process. Synthesize the " + "reference responses into concise, actionable guidance for the main " + "Hermes agent. Focus on next steps, tool-use strategy, risks, and any " + "disagreements. Do not answer the user directly unless that is all that " + "is needed; produce context the main agent should use in its normal loop.\n\n"
)

// Python str of a value behind an or default, the empty string for falsy values
func moaStr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Mapping view of a decoded config value, false for anything else
func moaMapOf(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// Copy of a row map one level deep
func moaCopyRow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Secrets through the central redactor then email and delimited phone shapes masked
func redactReferenceText(text any) any {
	s, ok := text.(string)
	if !ok || s == "" {
		return text
	}
	return redactReferenceString(s)
}

func redactReferenceString(s string) string {
	if s == "" {
		return s
	}
	s = tools.RedactSensitiveTextWith(nil, s, tools.RedactOptions{Force: true, CodeFile: true})
	if out, err := moaEmailRE.Replace(s, "[redacted email]", -1, -1); err == nil {
		s = out
	}
	if out, err := moaPhoneRE.Replace(s, "[redacted phone]", -1, -1); err == nil {
		s = out
	}
	return s
}

// Normalized privacy filter mode of a raw moa config block
func moaPrivacyMode(moaRaw any) string {
	raw, _ := moaMapOf(moaRaw)
	return provider.CoercePrivacyFilter(raw["privacy_filter"])
}

// Reference outputs with advisor text redacted and the accounting slot shared
func redactReferenceOutputs(outputs []moaRefOutput) []moaRefOutput {
	out := make([]moaRefOutput, len(outputs))
	for i, o := range outputs {
		out[i] = moaRefOutput{Label: o.Label, Text: redactReferenceString(o.Text), Acct: o.Acct}
	}
	return out
}

// Message content with a string or the text parts of a part list redacted
func redactMessageContent(content any) any {
	switch c := content.(type) {
	case string:
		return redactReferenceString(c)
	case []any:
		out := make([]any, len(c))
		for i, p := range c {
			if part, ok := p.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					cp := moaCopyRow(part)
					cp["text"] = redactReferenceString(text)
					out[i] = cp
					continue
				}
			}
			out[i] = p
		}
		return out
	case []map[string]any:
		out := make([]any, len(c))
		for i, part := range c {
			if text, ok := part["text"].(string); ok {
				cp := moaCopyRow(part)
				cp["text"] = redactReferenceString(text)
				out[i] = cp
				continue
			}
			out[i] = part
		}
		return out
	}
	return content
}

// Message copies for trace persistence with their content redacted
func redactTraceMessages(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		if m == nil {
			continue
		}
		cp := moaCopyRow(m)
		cp["content"] = redactMessageContent(m["content"])
		out[i] = cp
	}
	return out
}

// Accounting copy with its traced messages and output redacted
func redactTraceAccounting(acct *moaRefAccounting) *moaRefAccounting {
	if acct == nil {
		return nil
	}
	cp := *acct
	if rows, ok := acct.Messages.([]map[string]any); ok {
		cp.Messages = redactTraceMessages(rows)
	}
	cp.Output = redactReferenceText(acct.Output)
	return &cp
}

// Per reference usage, cost at the advisor's own rate and full trace, third slot of an output
type moaRefAccounting struct {
	Usage       provider.CanonicalUsage `json:"usage"`
	CostUSD     any                     `json:"cost_usd"`
	CostStatus  any                     `json:"cost_status"`
	CostSource  any                     `json:"cost_source"`
	Messages    any                     `json:"messages"`
	Output      any                     `json:"output"`
	Model       any                     `json:"model"`
	Provider    any                     `json:"provider"`
	Temperature any                     `json:"temperature"`
}

// One reference output as label, advisor text and accounting
type moaRefOutput struct {
	Label string
	Text  string
	Acct  *moaRefAccounting
}

var (
	moaPresetCacheMu sync.Mutex
	moaPresetCache   = map[string]map[string]any{}
)

// Resolved preset and raw moa block, the preset cached per config file stamp outside safe workers
func resolvePresetCached(ctx context.Context, presetName string) (map[string]any, any, error) {
	var stamp []any
	if config.WorkerConfigSnapshot() == nil {
		v, err := config.ConfigCacheVersionOrNone(ctx, home.ConfigPath(home.Dir(ctx)))
		if err != nil {
			return nil, nil, err
		}
		stamp = v
	}
	cfg, err := moaLoadConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	moaRaw := cfg["moa"]
	if !pyTruthy(moaRaw) {
		moaRaw = map[string]any{}
	}
	key := pyrepr.Repr(stamp) + "\x00" + presetName
	var preset map[string]any
	if stamp != nil {
		moaPresetCacheMu.Lock()
		preset = moaPresetCache[key]
		moaPresetCacheMu.Unlock()
	}
	if preset == nil {
		resolved, err := provider.ResolveMoAPreset(moaRaw, presetName)
		if err != nil {
			return nil, nil, err
		}
		preset = resolved
		if stamp != nil {
			moaPresetCacheMu.Lock()
			clear(moaPresetCache)
			moaPresetCache[key] = preset
			moaPresetCacheMu.Unlock()
		}
	}
	return preset, moaRaw, nil
}

type moaRuntimeEntry struct {
	at  time.Time
	out map[string]any
}

var (
	moaRuntimeCacheMu sync.Mutex
	moaRuntimeCache   = map[[3]string]moaRuntimeEntry{}
)

// Display label of a slot as provider:model with its reasoning effort suffix
func moaSlotLabel(slot map[string]any) string {
	label := pystr.Strip(moaStr(slot["provider"])) + ":" + pystr.Strip(moaStr(slot["model"]))
	if effort := pystr.Strip(moaStr(slot["reasoning_effort"])); effort != "" {
		return label + "[reasoning=" + effort + "]"
	}
	return label
}

// Reasoning config from the slot's own reasoning_effort, nil when unset or unparseable
func slotReasoningConfig(slot map[string]any) (out map[string]any) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return config.ParseReasoningEffort(slot["reasoning_effort"])
}

// Aggregator reasoning config from the slot, then the per model and global config
func aggregatorReasoningConfig(ctx context.Context, aggregator map[string]any) (out map[string]any) {
	if cfg := slotReasoningConfig(aggregator); cfg != nil {
		return cfg
	}
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	cfg, err := moaLoadConfig(ctx)
	if err != nil {
		return nil
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return config.ResolveReasoningConfig(cfg, moaStr(aggregator["model"]))
}

// Call kwargs of a slot with the provider's real api_mode, base_url and api_key, cached per profile with a TTL
func slotRuntime(ctx context.Context, slot map[string]any) map[string]any {
	prov := pystr.Strip(moaStr(slot["provider"]))
	model := pystr.Strip(moaStr(slot["model"]))
	key := [3]string{home.Key(home.Dir(ctx)), prov, model}
	now := moaNow()
	moaRuntimeCacheMu.Lock()
	entry, ok := moaRuntimeCache[key]
	moaRuntimeCacheMu.Unlock()
	if ok && now.Sub(entry.at) < moaRuntimeCacheTTL {
		return moaCopyRow(entry.out)
	}
	out := map[string]any{"provider": prov, "model": model}
	rt, err := moaResolveSlot(ctx, prov, model)
	if err != nil {
		moaLoopLog().Warn(fmt.Sprintf("MoA slot %s: provider '%s' could not be resolved (%s); calling with bare provider/model",
			moaSlotLabel(slot), prov, err))
		return out
	}
	if rt != nil {
		if rt.BaseURL != "" {
			out["base_url"] = rt.BaseURL
		}
		if rt.APIKey != "" {
			out["api_key"] = rt.APIKey
		}
		if rt.APIMode != "" {
			out["api_mode"] = string(rt.APIMode)
		}
		if extra, ok := rt.RequestOverrides["extra_body"].(map[string]any); ok && len(extra) > 0 {
			out["extra_body"] = moaCopyRow(extra)
		}
	}
	moaRuntimeCacheMu.Lock()
	moaRuntimeCache[key] = moaRuntimeEntry{at: now, out: out}
	moaRuntimeCacheMu.Unlock()
	return moaCopyRow(out)
}

// Slot extra_body defaults merged under the caller override, caller keys winning
func mergeSlotExtraBody(slotExtraBody, callerExtraBody any) any {
	slotBody, ok := slotExtraBody.(map[string]any)
	if !ok || len(slotBody) == 0 {
		return callerExtraBody
	}
	if caller, ok := callerExtraBody.(map[string]any); ok {
		out := moaCopyRow(slotBody)
		for k, v := range caller {
			out[k] = v
		}
		return out
	}
	if pyTruthy(callerExtraBody) {
		return callerExtraBody
	}
	return moaCopyRow(slotBody)
}

// Live agent cache disable flag and ttl, nil and empty without an agent
func agentCacheOpts(a *AIAgent) (*bool, string) {
	if a == nil {
		return nil, ""
	}
	disabled := a.cacheDisabled
	return &disabled, a.cacheTTL
}

// Runtime snapshot with the live agent's cache disable pinned, unchanged for a nil flag
func withCacheDisabled(runtime map[string]any, cacheDisabled *bool) map[string]any {
	if cacheDisabled == nil {
		return runtime
	}
	out := moaCopyRow(runtime)
	out["_cache_disabled"] = *cacheDisabled
	return out
}

// Aux progress hook stamping the agent activity clock on every advisor stream chunk
func referenceLivenessHook(a *AIAgent, label string) func() error {
	return func() error {
		if a != nil {
			a.TouchActivity("MoA reference " + label + ": stream progress")
		}
		return nil
	}
}

// Stamps a fan out milestone on the agent activity clock, swallowing any failure
func touchFanoutProgress(a *AIAgent, description string) {
	if a == nil {
		return
	}
	defer func() { _ = recover() }()
	a.TouchActivity(description)
}

// Advisor or aggregator request with cache_control applied when its route honours it, unchanged on any failure
func maybeApplyMoaCacheControl(ctx context.Context, messages []map[string]any, runtime map[string]any, cacheDisabled *bool, cacheTTL string) (out []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			moaLoopLog().Debug(fmt.Sprintf("MoA cache_control decoration skipped: %v", r))
			out = messages
		}
	}()
	if cacheDisabled == nil {
		if v, ok := runtime["_cache_disabled"]; ok && v != nil {
			flag := pyTruthy(v)
			cacheDisabled = &flag
		}
	}
	prov, model, baseURL, apiMode := moaStr(runtime["provider"]), moaStr(runtime["model"]), moaStr(runtime["base_url"]), moaStr(runtime["api_mode"])
	shouldCache, native := provider.AnthropicPromptCachePolicy(ctx, provider.BlankCachePolicyAgent(ctx, cacheDisabled),
		provider.CacheDestination{Provider: &prov, BaseURL: &baseURL, APIMode: &apiMode, Model: &model})
	if !shouldCache {
		return messages
	}
	return provider.ApplyAnthropicCacheControl(messages, provider.EffectiveCacheTTL(cacheTTL, model, prov), native, "",
		provider.EnvelopeToolPartCacheMarkersSupported(prov, baseURL))
}

// Field of a response payload or attribute object, nil when absent
func moaResponseField(response any, key string) any {
	if m, ok := response.(map[string]any); ok {
		return m[key]
	}
	return state.ToolCallField(response, key)
}

// Reference usage normalized for the slot's own provider and priced at its own rate, never failing
func priceReferenceResponse(ctx context.Context, response any, slot, runtime map[string]any) (usage provider.CanonicalUsage, cost, status, source any) {
	if raw := moaResponseField(response, "usage"); pyTruthy(raw) {
		func() {
			defer func() { _ = recover() }()
			usage = provider.NormalizeUsage(raw, moaStr(runtime["provider"]), moaStr(runtime["api_mode"]))
		}()
	}
	defer func() {
		if recover() != nil {
			cost, status, source = nil, nil, nil
		}
	}()
	result := provider.EstimateUsageCost(ctx, moaStr(slot["model"]), usage, moaStr(runtime["provider"]), moaStr(runtime["base_url"]), moaStr(runtime["api_key"]))
	if result.AmountUSD != nil {
		cost = result.AmountUSD
	}
	return usage, cost, result.Status, result.Source
}

// User initiated request headers of the slot provider attributed at that provider's endpoint, nil without a profile
func slotRequestHeaders(ctx context.Context, runtime map[string]any, a *AIAgent) map[string]string {
	prov := provider.NormalizeAuxProvider(ctx, moaStr(runtime["provider"]))
	if prov == "" {
		return nil
	}
	profile, ok := provider.GetProviderProfile(ctx, prov)
	if !ok {
		return nil
	}
	var scope any
	if a != nil {
		scope = a
	}
	answer := provider.RouteAnswer(ctx, "request_headers", prov, scope, map[string]any{
		"base_url": profile.BaseURL, "model": moaStr(runtime["model"]), "user_initiated": true})
	switch headers := answer.(type) {
	case map[string]string:
		return headers
	case map[string]any:
		out := make(map[string]string, len(headers))
		for k, v := range headers {
			out[k] = pyrepr.Str(v)
		}
		return out
	}
	return nil
}

// Options of one reference call
type moaRefCallOpts struct {
	temperature      *float64
	maxTokens        *int
	referenceTimeout *float64
	ctxLenCache      *moaCtxLenCache
	cacheDisabled    *bool
	cacheTTL         string
	agent            *AIAgent
}

func optFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func moaOptInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// One reference call as label, text and accounting, a failure becoming a labelled failed note
func runReference(ctx context.Context, slot map[string]any, refMessages []map[string]any, opts moaRefCallOpts) moaRefOutput {
	label := moaSlotLabel(slot)
	runtime := slotRuntime(ctx, slot)
	traceProvider := runtime["provider"]
	if !pyTruthy(traceProvider) {
		traceProvider = slot["provider"]
	}
	temperature := optFloat(opts.temperature)
	messages := append([]map[string]any{{"role": "system", "content": moaReferenceSystemPrompt}}, refMessages...)
	out, err := func() (out moaRefOutput, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		trimmed := trimMessagesForReference(ctx, messages, slot, runtime, opts.maxTokens, opts.ctxLenCache)
		trimmed = maybeApplyMoaCacheControl(ctx, trimmed, withCacheDisabled(runtime, opts.cacheDisabled), nil, opts.cacheTTL)
		kwargs := map[string]any{
			"task": "moa_reference", "messages": trimmed, "temperature": temperature, "max_tokens": moaOptInt(opts.maxTokens),
			"timeout": optFloat(opts.referenceTimeout), "reasoning_config": slotReasoningConfig(slot),
			"extra_headers": slotRequestHeaders(ctx, runtime, opts.agent),
		}
		for k, v := range runtime {
			kwargs[k] = v
		}
		callCtx := ctx
		if opts.agent != nil {
			callCtx = provider.AuxProgressHook(ctx, referenceLivenessHook(opts.agent, label))
		}
		response, err := moaCallLLM(callCtx, kwargs)
		if err != nil {
			return out, err
		}
		text := extractMoaText(response)
		if text == "" {
			text = "(empty response)"
		}
		usage, cost, status, source := priceReferenceResponse(ctx, response, slot, runtime)
		acct := &moaRefAccounting{Usage: usage, CostUSD: cost, CostStatus: status, CostSource: source, Messages: trimmed,
			Output: text, Model: slot["model"], Provider: traceProvider, Temperature: temperature}
		return moaRefOutput{Label: label, Text: text, Acct: acct}, nil
	}()
	if err == nil {
		return out
	}
	moaLoopLog().Warn(fmt.Sprintf("MoA reference model %s failed: %s", label, err))
	note := "[failed: " + err.Error() + "]"
	return moaRefOutput{Label: label, Text: note, Acct: &moaRefAccounting{Messages: messages, Output: note, Model: slot["model"],
		Provider: traceProvider, Temperature: temperature}}
}

// Context window memo per provider and model shared by one fan out, failures memoized as zero
type moaCtxLenCache struct {
	mu sync.Mutex
	m  map[[2]string]int
}

func newMoaCtxLenCache() *moaCtxLenCache { return &moaCtxLenCache{m: map[[2]string]int{}} }

// Context window of a slot memoized per provider and model, zero when unresolvable
func referenceContextLength(ctx context.Context, slot, runtime map[string]any, cache *moaCtxLenCache) int {
	model := moaStr(slot["model"])
	provRaw := runtime["provider"]
	if !pyTruthy(provRaw) {
		provRaw = slot["provider"]
	}
	prov := moaStr(provRaw)
	key := [2]string{prov, model}
	if cache != nil {
		cache.mu.Lock()
		n, ok := cache.m[key]
		cache.mu.Unlock()
		if ok {
			return n
		}
	}
	n := func() (n int) {
		defer func() {
			if recover() != nil {
				n = 0
			}
		}()
		n, err := moaGetModelContextLength(ctx, provider.ContextLengthRequest{Model: model, BaseURL: moaStr(runtime["base_url"]),
			APIKey: moaStr(runtime["api_key"]), Provider: prov})
		if err != nil {
			moaLoopLog().Debug("MoA reference context-length resolution failed for " + moaSlotLabel(slot))
			return 0
		}
		return n
	}()
	if cache != nil {
		cache.mu.Lock()
		cache.m[key] = n
		cache.mu.Unlock()
	}
	return n
}

// Advisory request trimmed oldest first to the reference window, keeping a user first body and the trailing turns
func trimMessagesForReference(ctx context.Context, messages []map[string]any, slot, runtime map[string]any, reserveOutputTokens *int, cache *moaCtxLenCache) []map[string]any {
	if len(messages) == 0 || !pyTruthy(slot["model"]) {
		return messages
	}
	contextLength := referenceContextLength(ctx, slot, runtime, cache)
	if contextLength <= 0 {
		return messages
	}
	reserve := moaReferenceDefaultOutputReserve
	if reserveOutputTokens != nil && *reserveOutputTokens > 0 {
		reserve = *reserveOutputTokens
	}
	budget := int(float64(contextLength)*(1.0-moaReferenceTrimSafetyFraction)) - reserve
	estimated := moaEstimateMessagesTokensRough(ctx, messages)
	if budget <= 0 || estimated <= budget {
		return messages
	}
	headCount := 0
	if messages[0]["role"] == "system" {
		headCount = 1
	}
	weights := make([]int, len(messages))
	total := 0
	for i, m := range messages {
		weights[i] = moaEstimateMessagesTokensRough(ctx, []map[string]any{m})
		total += weights[i]
	}
	start := headCount
	remaining := len(messages) - headCount
	for remaining > 2 && total > budget {
		total -= weights[start]
		start++
		remaining--
		for remaining > 2 && messages[start]["role"] == "assistant" {
			total -= weights[start]
			start++
			remaining--
		}
	}
	for remaining > 1 && messages[start]["role"] == "assistant" {
		total -= weights[start]
		start++
		remaining--
	}
	trimmed := append(append([]map[string]any{}, messages[:headCount]...), messages[start:]...)
	if dropped := len(messages) - len(trimmed); dropped > 0 {
		moaLoopLog().Info(fmt.Sprintf("MoA reference %s: estimated %d tokens exceeds budget %d (window %d, output reserve %d); dropped %d oldest message(s).",
			moaSlotLabel(slot), estimated, budget, contextLength, reserve, dropped))
	}
	return trimmed
}

// Output of a slot that was not fully run, with zero accounting
func placeholderOutput(slot map[string]any, note string) moaRefOutput {
	return moaRefOutput{Label: moaSlotLabel(slot), Text: note, Acct: &moaRefAccounting{}}
}

// Fan out options of one parallel reference run
type moaFanoutOpts struct {
	temperature      *float64
	maxTokens        *int
	progress         func(done, total int, label string)
	referenceTimeout *float64
	agent            *AIAgent
	lateSink         func(label string, acct *moaRefAccounting)
}

const (
	moaFutureQueued = iota
	moaFutureRunning
	moaFutureDone
	moaFutureCancelled
)

// One dispatched reference call and the done callback an interrupt attaches to it
type moaFuture struct {
	mu     sync.Mutex
	state  int
	result moaRefOutput
	late   func(moaRefOutput)
}

// Unfinished slots after an interrupt filled, queued ones cancelled and running ones handed to the late sink
func settleInterrupted(futures map[int]*moaFuture, results []*moaRefOutput, referenceModels []map[string]any, lateSink func(string, *moaRefAccounting)) {
	indexes := make([]int, 0, len(futures))
	for idx := range futures {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		if results[idx] != nil {
			continue
		}
		f := futures[idx]
		f.mu.Lock()
		switch f.state {
		case moaFutureDone:
			r := f.result
			results[idx] = &r
		case moaFutureQueued:
			f.state = moaFutureCancelled
			p := placeholderOutput(referenceModels[idx], moaInterruptedReferenceNote)
			results[idx] = &p
		default:
			p := placeholderOutput(referenceModels[idx], moaInterruptedReferenceNote)
			results[idx] = &p
			if lateSink != nil {
				label := p.Label
				f.late = func(r moaRefOutput) { recordLate(lateSink, label, r) }
			}
		}
		f.mu.Unlock()
	}
}

// Forwards an abandoned reference's real accounting to the sink, swallowing sink failures
func recordLate(sink func(string, *moaRefAccounting), label string, r moaRefOutput) {
	defer func() {
		if recover() != nil {
			moaLoopLog().Debug("MoA: late accounting sink failed for " + label)
		}
	}()
	sink(label, r.Acct)
}

// All reference models fanned out in parallel, outputs in slot order, interruptible through the agent
func runReferencesParallel(ctx context.Context, referenceModels []map[string]any, refMessages []map[string]any, opts moaFanoutOpts) []moaRefOutput {
	if len(referenceModels) == 0 {
		return nil
	}
	total := len(referenceModels)
	results := make([]*moaRefOutput, total)
	futures := map[int]*moaFuture{}
	sem := make(chan struct{}, min(moaMaxReferenceWorkers, total))
	doneCh := make(chan int, total)
	ctxLenCache := newMoaCtxLenCache()
	cacheDisabled, cacheTTL := agentCacheOpts(opts.agent)
	completed := 0
	collect := func(idx int) {
		f := futures[idx]
		f.mu.Lock()
		r := f.result
		f.mu.Unlock()
		results[idx] = &r
		completed++
		touchFanoutProgress(opts.agent, fmt.Sprintf("MoA: %d of %d references complete", completed, total))
		if opts.progress != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						moaLoopLog().Debug(fmt.Sprintf("MoA progress_callback failed: %v", r))
					}
				}()
				opts.progress(completed, total, moaSlotLabel(referenceModels[idx]))
			}()
		}
	}
	callOpts := moaRefCallOpts{temperature: opts.temperature, maxTokens: opts.maxTokens, referenceTimeout: opts.referenceTimeout,
		ctxLenCache: ctxLenCache, cacheDisabled: cacheDisabled, cacheTTL: cacheTTL, agent: opts.agent}
	for idx, slot := range referenceModels {
		if slot["provider"] == "moa" {
			p := placeholderOutput(slot, moaRecursiveReferenceNote)
			results[idx] = &p
			continue
		}
		f := &moaFuture{}
		futures[idx] = f
		go func(idx int, slot map[string]any, f *moaFuture) {
			sem <- struct{}{}
			defer func() { <-sem }()
			f.mu.Lock()
			if f.state != moaFutureQueued {
				f.mu.Unlock()
				return
			}
			f.state = moaFutureRunning
			f.mu.Unlock()
			out := runReference(ctx, slot, refMessages, callOpts)
			f.mu.Lock()
			f.result, f.state = out, moaFutureDone
			late := f.late
			f.mu.Unlock()
			doneCh <- idx
			if late != nil {
				late(out)
			}
		}(idx, slot, f)
	}
	pending := len(futures)
	timer := time.NewTimer(moaReferencePollInterval)
	defer timer.Stop()
wait:
	for pending > 0 {
		select {
		case idx := <-doneCh:
			collect(idx)
			pending--
		case <-timer.C:
			if pending > 0 && opts.agent != nil && opts.agent.IsInterrupted() {
				settle := time.NewTimer(moaReferenceInterruptSettle)
			drain:
				for pending > 0 {
					select {
					case idx := <-doneCh:
						collect(idx)
						pending--
					case <-settle.C:
						break drain
					}
				}
				settle.Stop()
				settleInterrupted(futures, results, referenceModels, opts.lateSink)
				break wait
			}
			timer.Reset(moaReferencePollInterval)
		}
	}
	out := make([]moaRefOutput, 0, total)
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// Head and tail preview of a tool result with an omitted characters marker
func truncateToolResult(text string, budget int) string {
	runes := []rune(text)
	if text == "" || len(runes) <= budget {
		return text
	}
	half := budget / 2
	omitted := len(runes) - 2*half
	return fmt.Sprintf("%s\n[... %d chars omitted ...]\n%s", string(runes[:half]), omitted, string(runes[len(runes)-half:]))
}

// Elements of a list shaped value, nil for anything else
func moaListItems(v any) []any {
	switch l := v.(type) {
	case nil:
		return nil
	case []any:
		return l
	case string, []byte:
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// Assistant tool calls rendered as called tool lines over dict and attribute shapes
func renderToolCalls(toolCalls any) string {
	var lines []string
	for _, tc := range moaListItems(toolCalls) {
		fn := state.ToolCallField(tc, "function")
		name := state.ToolCallField(fn, "name")
		if !pyTruthy(name) {
			name = state.ToolCallField(tc, "name")
		}
		if !pyTruthy(name) {
			name = "tool"
		}
		argsText := ""
		switch args := state.ToolCallField(fn, "arguments").(type) {
		case nil:
		case string:
			argsText = args
		default:
			if dumped, err := pyjson.Dumps(args, pyjson.EnsureASCII(false)); err == nil {
				argsText = dumped
			} else {
				argsText = pyrepr.Str(args)
			}
		}
		if argsText != "" {
			lines = append(lines, "[called tool: "+pyrepr.Str(name)+"("+argsText+")]")
		} else {
			lines = append(lines, "[called tool: "+pyrepr.Str(name)+"]")
		}
	}
	return strings.Join(lines, "\n")
}

// True when the acting model already called tools since the last real user turn
func toolActivitySinceLastUser(messages []map[string]any) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		role := messages[i]["role"]
		if role == "user" {
			return false
		}
		if role == "tool" || (role == "assistant" && pyTruthy(messages[i]["tool_calls"])) {
			return true
		}
	}
	return false
}

// Sorted union of two data use requirement lists
func mergeDataUse(existing, add any) []any {
	set := map[string]bool{}
	for _, v := range moaListItems(existing) {
		set[pyrepr.Str(v)] = true
	}
	for _, v := range moaListItems(add) {
		set[pyrepr.Str(v)] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// Advisory view of the transcript as plain user and assistant text turns ending on a user turn
func referenceMessages(messages []map[string]any) []map[string]any {
	var rendered []map[string]any
	var lastUserContent *string
	for _, msg := range messages {
		role := msg["role"]
		content := msg["content"]
		text := state.FlattenMessageText(content, "\n")
		switch role {
		case "user":
			if pystr.Strip(text) == "" && len(moaListItems(content)) > 0 {
				text = moaImageOnlyUserNote
			}
			if pystr.Strip(text) == "" {
				continue
			}
			t := text
			lastUserContent = &t
			rendered = append(rendered, map[string]any{"role": "user", "content": text})
		case "assistant":
			var parts []string
			if stripped := pystr.Strip(text); stripped != "" {
				parts = append(parts, stripped)
			}
			if calls := renderToolCalls(msg["tool_calls"]); calls != "" {
				parts = append(parts, calls)
			}
			if len(parts) > 0 {
				rendered = append(rendered, map[string]any{"role": "assistant", "content": strings.Join(parts, "\n")})
			}
		case "tool":
			block := "[tool result: " + truncateToolResult(text, moaReferenceToolResultBudget) + "]"
			if n := len(rendered); n > 0 && rendered[n-1]["role"] == "assistant" {
				rendered[n-1]["content"] = pyrepr.Str(rendered[n-1]["content"]) + "\n" + block
			} else {
				rendered = append(rendered, map[string]any{"role": "assistant", "content": block})
			}
			if pyTruthy(msg[state.DataUse]) {
				last := rendered[len(rendered)-1]
				last[state.DataUse] = mergeDataUse(last[state.DataUse], msg[state.DataUse])
			}
		}
	}
	if n := len(rendered); n > 0 && rendered[n-1]["role"] == "assistant" {
		rendered = append(rendered, map[string]any{"role": "user", "content": moaAdvisoryInstruction})
	}
	if len(rendered) == 0 {
		if lastUserContent != nil {
			return []map[string]any{{"role": "user", "content": *lastUserContent}}
		}
		for i := len(messages) - 1; i >= 0; i-- {
			fallback := ""
			if messages[i]["role"] == "user" {
				fallback = state.FlattenMessageText(messages[i]["content"], "\n")
			}
			if pystr.Strip(fallback) != "" {
				return []map[string]any{{"role": "user", "content": fallback}}
			}
		}
	}
	return rendered
}

// Assistant text of a completed response, transport normalized first and choices zero second
func extractMoaText(response any) string {
	text := func() (text string) {
		defer func() {
			if recover() != nil {
				text = ""
			}
		}()
		norm, err := provider.GetTransport("chat_completions").NormalizeResponse(response, nil)
		if err != nil || norm == nil || norm.Message == nil {
			return ""
		}
		s, _ := norm.Message.Content.(string)
		return pystr.Strip(s)
	}()
	if text != "" {
		return text
	}
	defer func() {
		if recover() != nil {
			text = ""
		}
	}()
	choices := moaListItems(moaResponseField(response, "choices"))
	if len(choices) == 0 {
		return ""
	}
	message := state.ToolCallField(choices[0], "message")
	content := state.ToolCallField(message, "content")
	s, ok := content.(string)
	if !ok {
		s = moaStr(content)
	}
	return pystr.Strip(s)
}

// Optional preset temperature, nil for absent, blank, null or non numeric values
func presetTemperature(preset map[string]any, key string) *float64 {
	value := preset[key]
	if value == nil {
		return nil
	}
	if s, ok := value.(string); ok && pystr.Strip(s) == "" {
		return nil
	}
	f, err := pyint.Float(value)
	if err != nil {
		moaLoopLog().Warn(fmt.Sprintf("ignoring non-numeric %s=%s in MoA preset", key, pyrepr.Repr(value)))
		return nil
	}
	return &f
}

// Hex sha256 over the role and content of every message joined by NUL
func hashMessages(msgs []map[string]any) string {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = pyrepr.Str(m["role"]) + ":" + pyrepr.Str(m["content"])
	}
	sum := sha256.Sum256([]byte(strings.ToValidUTF8(strings.Join(parts, "\x00"), "?")))
	return hex.EncodeToString(sum[:])
}

// True for a failed or skipped reference sentinel
func isFailedReference(text string) bool {
	lowered := pystr.Lower(pystr.Lstrip(text))
	return strings.HasPrefix(lowered, "[failed:") || strings.HasPrefix(lowered, "[skipped:")
}

// Numbered reference blocks for the aggregator with any degraded notice appended
func joinReferenceOutputs(outputs []moaRefOutput, degraded string) string {
	blocks := make([]string, len(outputs))
	for i, o := range outputs {
		blocks[i] = fmt.Sprintf("Reference %d — %s:\n%s", i+1, o.Label, o.Text)
	}
	joined := strings.Join(blocks, "\n\n")
	if degraded != "" {
		if joined != "" {
			return joined + "\n\n" + degraded
		}
		return degraded
	}
	return joined
}

// Fan out usage and dollar cost summed with every advisor priced at its own rate, nil cost when none priced
func sumReferenceAccounting(outputs []moaRefOutput) (provider.CanonicalUsage, *big.Rat) {
	usage := provider.CanonicalUsage{}
	var cost *big.Rat
	for _, o := range outputs {
		if o.Acct == nil {
			continue
		}
		usage = usage.Add(o.Acct.Usage)
		if c, ok := o.Acct.CostUSD.(*big.Rat); ok && c != nil {
			if cost == nil {
				cost = new(big.Rat)
			}
			cost = new(big.Rat).Add(cost, c)
		}
	}
	return usage, cost
}

// Notice naming unavailable references, empty under the silent policy
func degradedNotice(failedLabels []string, policy string) string {
	if len(failedLabels) == 0 || pystr.Lower(pystr.Strip(policy)) == "silent" {
		return ""
	}
	return "[Reference models unavailable: " + strings.Join(failedLabels, ", ") + "]"
}

func slotLabels(slots []map[string]any) string {
	labels := make([]string, len(slots))
	for i, s := range slots {
		labels[i] = moaSlotLabel(s)
	}
	return strings.Join(labels, ", ")
}

// Advisor outputs for the aggregator, the degraded notice and whether every reference failed
func guidanceInputs(outputs []moaRefOutput, privacyFull bool, policy string) ([]moaRefOutput, string, bool) {
	var successful []moaRefOutput
	var failedLabels []string
	for _, o := range outputs {
		if isFailedReference(o.Text) {
			failedLabels = append(failedLabels, o.Label)
		} else {
			successful = append(successful, o)
		}
	}
	aggRefs := successful
	if privacyFull {
		aggRefs = redactReferenceOutputs(successful)
	}
	return aggRefs, degradedNotice(failedLabels, policy), len(outputs) > 0 && len(successful) == 0
}

// Enabled reference slots of a preset list, non mapping entries dropped
func enabledSlots(raw any) []map[string]any {
	var out []map[string]any
	for _, item := range moaListItems(raw) {
		slot, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if enabled, present := slot["enabled"]; present && !pyTruthy(enabled) {
			continue
		}
		out = append(out, slot)
	}
	return out
}

// One shot MoA guidance from the reference fan out synthesized by the aggregator, failures becoming notes
func aggregateMoaContext(ctx context.Context, a *AIAgent, userPrompt string, apiMessages []map[string]any, referenceModels []any, aggregator map[string]any, temperature, aggregatorTemperature, referenceTimeout *float64, referenceMaxTokens *int, degradedReferencePolicy string) (string, error) {
	slots := enabledSlots(referenceModels)
	outputs := runReferencesParallel(ctx, slots, referenceMessages(apiMessages), moaFanoutOpts{temperature: temperature,
		maxTokens: referenceMaxTokens, referenceTimeout: referenceTimeout, agent: a})
	privacyFull := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				moaLoopLog().Debug(fmt.Sprintf("MoA privacy filter check failed: %v", r))
			}
		}()
		cfg, err := moaLoadConfig(ctx)
		if err != nil {
			moaLoopLog().Debug(fmt.Sprintf("MoA privacy filter check failed: %v", err))
			return
		}
		privacyFull = moaPrivacyMode(cfg["moa"]) == "full"
	}()
	aggRefs, degraded, allFailed := guidanceInputs(outputs, privacyFull, degradedReferencePolicy)
	joined := joinReferenceOutputs(aggRefs, degraded)
	if allFailed {
		moaLoopLog().Warn(fmt.Sprintf("MoA: all %d reference(s) failed — skipping aggregator synthesis", len(outputs)))
		notice := degraded
		if notice == "" {
			notice = "[Reference models unavailable]"
		}
		return moaAllFailedAggregateHeader + "References: " + slotLabels(slots) + "\n\n" + notice, nil
	}
	synthPrompt := moaAggregatorSynthesisInstruction + "Original user prompt:\n" + userPrompt + "\n\n" + "Reference responses:\n" + joined
	aggLabel := moaSlotLabel(aggregator)
	aggRuntime := slotRuntime(ctx, aggregator)
	touchFanoutProgress(a, "MoA: synthesizing aggregator guidance ("+aggLabel+")")
	kwargs := map[string]any{"task": "moa_aggregator", "messages": []map[string]any{{"role": "user", "content": synthPrompt}},
		"temperature": optFloat(aggregatorTemperature), "reasoning_config": aggregatorReasoningConfig(ctx, aggregator)}
	for k, v := range aggRuntime {
		kwargs[k] = v
	}
	synthesis := ""
	if response, err := moaCallLLM(ctx, kwargs); err != nil {
		moaLoopLog().Warn(fmt.Sprintf("MoA aggregator model %s failed: %s", aggLabel, err))
	} else {
		synthesis = extractMoaText(response)
	}
	body := synthesis
	if body == "" {
		body = joined
	}
	return moaAggregateContextHeader + "Aggregator: " + aggLabel + "\n" + "References: " + slotLabels(slots) + "\n\n" + pystr.Strip(body), nil
}

// Completed response adapted into one delta stream chunk at the facade boundary
func completedResponseAsStreamChunk(response map[string]any) map[string]any {
	choices := moaListItems(response["choices"])
	var first any
	if len(choices) > 0 {
		first = choices[0]
	}
	message := state.ToolCallField(first, "message")
	var toolCallDeltas any
	if raw := moaListItems(state.ToolCallField(message, "tool_calls")); len(raw) > 0 {
		deltas := make([]any, len(raw))
		for i, tc := range raw {
			index := state.ToolCallField(tc, "index")
			if index == nil {
				index = i
			}
			typ := state.ToolCallField(tc, "type")
			if !pyTruthy(typ) {
				typ = "function"
			}
			fn := state.ToolCallField(tc, "function")
			deltas[i] = map[string]any{"index": index, "id": state.ToolCallField(tc, "id"), "type": typ,
				"function": map[string]any{"name": state.ToolCallField(fn, "name"), "arguments": state.ToolCallField(fn, "arguments")}}
		}
		toolCallDeltas = deltas
	}
	delta := map[string]any{"content": state.ToolCallField(message, "content"), "tool_calls": toolCallDeltas}
	for _, k := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
		delta[k] = state.ToolCallField(message, k)
	}
	index := state.ToolCallField(first, "index")
	if index == nil {
		index = 0
	}
	finish := state.ToolCallField(first, "finish_reason")
	if !pyTruthy(finish) {
		finish = "stop"
	}
	return map[string]any{"id": response["id"], "model": response["model"],
		"choices": []any{map[string]any{"index": index, "delta": delta, "finish_reason": finish}}, "usage": response["usage"]}
}

// One chunk stream over a completed aggregator response returned for a streaming request
type moaOneChunkStream struct {
	chunk map[string]any
	sent  bool
}

func (s *moaOneChunkStream) Next(context.Context) (map[string]any, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return s.chunk, nil
}

func (s *moaOneChunkStream) Close() error {
	s.sent = true
	return nil
}

// Turn varying reference block appended as its own trailing user message
func attachReferenceGuidance(aggMessages []map[string]any, guidance string) []map[string]any {
	return append(aggMessages, map[string]any{"role": "user", "content": guidance})
}

// Exact inverse of attaching guidance over the plain or single text part form, inputs untouched
func peelReferenceGuidance(messages []map[string]any, guidance any) []map[string]any {
	if !pyTruthy(guidance) || len(messages) == 0 {
		return messages
	}
	text := pyrepr.Str(guidance)
	last := messages[len(messages)-1]
	if last == nil || last["role"] != "user" {
		return messages
	}
	peeled := func() []map[string]any { return append([]map[string]any{}, messages[:len(messages)-1]...) }
	content := last["content"]
	if s, ok := content.(string); ok && s == text {
		return peeled()
	}
	if parts := moaListItems(content); len(parts) == 1 && content != nil {
		if _, isStr := content.(string); !isStr {
			var part map[string]any
			switch p := parts[0].(type) {
			case map[string]any:
				part = p
			}
			if part != nil {
				typ, present := part["type"]
				if !present {
					typ = "text"
				}
				partText := part["text"]
				if !pyTruthy(partText) {
					partText = ""
				}
				if s, ok := partText.(string); ok && typ == "text" && s == text {
					return peeled()
				}
			}
		}
	}
	return messages
}

// Turn scoped reference cache key, nil standing for no key
type moaFanoutKey struct {
	preset string
	hash   string
	labels []string
}

func (k *moaFanoutKey) equal(o *moaFanoutKey) bool {
	if k == nil || o == nil {
		return k == o
	}
	if k.preset != o.preset || k.hash != o.hash || len(k.labels) != len(o.labels) {
		return false
	}
	for i := range k.labels {
		if k.labels[i] != o.labels[i] {
			return false
		}
	}
	return true
}

// Full turn trace parts from a cache miss create awaiting the flush
type moaPendingTrace struct {
	preset                string
	referenceOutputs      []moaRefOutput
	aggregatorSlot        map[string]any
	aggregatorTemperature any
	aggregatorLabel       string
	inputMessages         any
	hasInputMessages      bool
	output                *string
	streamed              bool
}

// MoAChatCompletions is the chat completions facade whose aggregator is the acting model
type MoAChatCompletions struct {
	PresetName        string
	referenceCallback func(event string, kwargs map[string]any)
	agent             *AIAgent

	refCacheKey     *moaFanoutKey
	refCacheOutputs []moaRefOutput

	accountingMu          sync.Mutex
	pendingReferenceUsage provider.CanonicalUsage
	pendingReferenceCost  *big.Rat

	lastAggregatorSlot   map[string]any
	pendingTrace         *moaPendingTrace
	lastReferenceMetrics []any

	fanoutIterationCount int
	fanoutTurnSig        string
	fanoutLastStateSig   string
	privacyMode          string

	mergeSameRoleDestinations map[[2]string]bool
}

// NewMoAChatCompletions builds the facade for a preset with its display relay and owning agent
func NewMoAChatCompletions(presetName string, referenceCallback func(event string, kwargs map[string]any), a *AIAgent) *MoAChatCompletions {
	if presetName == "" {
		presetName = "default"
	}
	return &MoAChatCompletions{PresetName: presetName, referenceCallback: referenceCallback, agent: a,
		mergeSameRoleDestinations: map[[2]string]bool{}}
}

// Pops pending fan out usage and cost and resets both so a retry cannot double count
func (c *MoAChatCompletions) consumeReferenceUsage() (provider.CanonicalUsage, *big.Rat) {
	c.accountingMu.Lock()
	defer c.accountingMu.Unlock()
	usage, cost := c.pendingReferenceUsage, c.pendingReferenceCost
	c.pendingReferenceUsage, c.pendingReferenceCost = provider.CanonicalUsage{}, nil
	return usage, cost
}

// Done callback of an abandoned still billing reference folding its real spend in
func (c *MoAChatCompletions) recordLateReferenceAccounting(label string, acct *moaRefAccounting) {
	if acct == nil {
		return
	}
	c.foldPendingAccounting(sumReferenceAccounting([]moaRefOutput{{Label: label, Acct: acct}}))
	moaLoopLog().Debug("MoA: recorded late accounting for interrupted reference " + label)
}

// Adds fan out spend to the pending totals so late interrupted deposits survive
func (c *MoAChatCompletions) foldPendingAccounting(usage provider.CanonicalUsage, cost *big.Rat) {
	c.accountingMu.Lock()
	defer c.accountingMu.Unlock()
	c.pendingReferenceUsage = c.pendingReferenceUsage.Add(usage)
	if cost != nil {
		if c.pendingReferenceCost == nil {
			c.pendingReferenceCost = new(big.Rat)
		}
		c.pendingReferenceCost = new(big.Rat).Add(c.pendingReferenceCost, cost)
	}
}

// Flushes the pending full turn trace to disk, a no op when nothing is pending
func (c *MoAChatCompletions) consumeAndSaveTrace(ctx context.Context, sessionID string, aggregatorOutputFallback *string) {
	pending := c.pendingTrace
	c.pendingTrace = nil
	if pending == nil || !pending.hasInputMessages {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			moaLoopLog().Debug(fmt.Sprintf("MoA trace flush failed: %v", r))
		}
	}()
	aggSlot := pending.aggregatorSlot
	output := pending.output
	if output == nil && aggregatorOutputFallback != nil && *aggregatorOutputFallback != "" {
		output = aggregatorOutputFallback
	}
	refs := make([]MoAReference, len(pending.referenceOutputs))
	for i, r := range pending.referenceOutputs {
		refs[i] = MoAReference{Label: r.Label, Text: r.Text}
		if r.Acct != nil {
			refs[i].Acct = r.Acct
		}
	}
	SaveMoATurn(ctx, MoATurn{SessionID: sessionID, PresetName: pending.preset, ReferenceOutputs: refs,
		AggregatorLabel: pending.aggregatorLabel, AggregatorModel: aggSlot["model"], AggregatorProvider: aggSlot["provider"],
		AggregatorTemperature: pending.aggregatorTemperature, AggregatorInputMessages: pending.inputMessages,
		AggregatorOutput: output, AggregatorStreamed: pending.streamed})
}

// Best effort display event, a failing callback never breaking the turn
func (c *MoAChatCompletions) emit(event string, kwargs map[string]any) {
	if c.referenceCallback == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			moaLoopLog().Debug(fmt.Sprintf("MoA reference_callback failed for %s: %v", event, r))
		}
	}()
	c.referenceCallback(event, kwargs)
}

// Advisor fan out run and the exact aggregator request returned for the loop to measure and hand back
func (c *MoAChatCompletions) Prepare(ctx context.Context, messages []map[string]any) (map[string]any, error) {
	out, err := c.Create(ctx, map[string]any{"messages": messages, "_moa_prepare_only": true})
	if err != nil {
		return nil, err
	}
	prepared, _ := out.(map[string]any)
	return prepared, nil
}

// Already generated guidance re attached to a rebuilt transcript
func (c *MoAChatCompletions) RebasePreparedRequest(prepared map[string]any, messages []map[string]any) map[string]any {
	aggMessages := make([]map[string]any, len(messages))
	for i, m := range messages {
		aggMessages[i] = moaCopyRow(m)
	}
	if guidance := prepared["guidance"]; pyTruthy(guidance) {
		aggMessages = attachReferenceGuidance(aggMessages, pyrepr.Str(guidance))
	}
	out := moaCopyRow(prepared)
	out["messages"] = aggMessages
	return out
}

// Aggregator request cache breakpointed for its destination with the guidance peeled around planning
func (c *MoAChatCompletions) planAggregatorCache(ctx context.Context, aggMessages, tools []map[string]any, guidance any, aggRuntime map[string]any) (outMessages, outTools []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			moaLoopLog().Warn(fmt.Sprintf("MoA aggregator cache plan failed — sending undecorated request (cache misses expected): %v", r))
			outMessages, outTools = aggMessages, tools
		}
	}()
	planning := aggMessages
	if pyTruthy(guidance) {
		planning = peelReferenceGuidance(aggMessages, pyrepr.Str(guidance))
	}
	cacheDisabled, cacheTTL := agentCacheOpts(c.agent)
	var static *string
	if c.agent != nil && c.agent.cachedSystemPromptStatic != "" {
		s := c.agent.cachedSystemPromptStatic
		static = &s
	}
	planned, plannedTools := provider.PlanCacheSectionsForDestination(ctx, planning, tools, moaStr(aggRuntime["provider"]),
		moaStr(aggRuntime["base_url"]), moaStr(aggRuntime["api_mode"]), moaStr(aggRuntime["model"]), cacheDisabled, cacheTTL, static)
	if pyTruthy(guidance) {
		planned = attachReferenceGuidance(planned, pyrepr.Str(guidance))
	}
	return planned, plannedTools
}

// Sends an already prepared aggregator request once, retrying merged after a strict alternation rejection
func (c *MoAChatCompletions) callPreparedAggregator(ctx context.Context, prepared, apiKwargs map[string]any) (any, error) {
	aggRaw, ok := prepared["aggregator"]
	if !ok {
		return nil, errors.New("KeyError: 'aggregator'")
	}
	aggregator, _ := aggRaw.(map[string]any)
	if aggregator["provider"] == "moa" {
		return nil, errors.New("MoA aggregator cannot be another MoA preset")
	}
	rawMessages, ok := prepared["messages"]
	if !ok {
		return nil, errors.New("KeyError: 'messages'")
	}
	aggRuntime := slotRuntime(ctx, aggregator)
	aggMessages, tools := c.planAggregatorCache(ctx, mapRows(rawMessages), mapRows(apiKwargs["tools"]), prepared["guidance"], aggRuntime)
	trace := c.pendingTrace
	if trace != nil {
		trace.aggregatorLabel = moaSlotLabel(aggregator)
	}
	stream := pyTruthy(apiKwargs["stream"])
	streamKwargs := map[string]any{}
	if stream {
		options := apiKwargs["stream_options"]
		if !pyTruthy(options) {
			options = map[string]any{"include_usage": true}
		}
		streamKwargs["stream"], streamKwargs["stream_options"] = true, options
		if apiKwargs["timeout"] != nil {
			streamKwargs["timeout"] = apiKwargs["timeout"]
		}
	}
	aggExtraBody := mergeSlotExtraBody(aggRuntime["extra_body"], apiKwargs["extra_body"])
	delete(aggRuntime, "extra_body")
	destination := destinationKey(aggRuntime)
	if c.mergeSameRoleDestinations == nil {
		c.mergeSameRoleDestinations = map[[2]string]bool{}
	}
	merged := c.mergeSameRoleDestinations[destination]
	if merged {
		aggMessages, _ = mergeSameRoleMessages(aggMessages)
	}
	temperature, ok := prepared["aggregator_temperature"]
	if !ok {
		return nil, errors.New("KeyError: 'aggregator_temperature'")
	}
	var toolsArg any
	if tools != nil {
		toolsArg = tools
	}
	reasoning := aggregatorReasoningConfig(ctx, aggregator)
	send := func(messages []map[string]any) (any, error) {
		kwargs := map[string]any{"task": "moa_aggregator", "temperature": temperature, "max_tokens": apiKwargs["max_tokens"],
			"tools": toolsArg, "extra_body": aggExtraBody, "reasoning_config": reasoning, "messages": messages}
		for k, v := range streamKwargs {
			kwargs[k] = v
		}
		for k, v := range aggRuntime {
			kwargs[k] = v
		}
		return moaCallLLM(ctx, kwargs)
	}
	response, err := send(aggMessages)
	if err != nil {
		if merged {
			return nil, err
		}
		retry, changed := mergeSameRoleMessages(aggMessages)
		if !changed || !isRoleAlternationRejection(err, aggRuntime) {
			return nil, err
		}
		c.mergeSameRoleDestinations[destination] = true
		moaLoopLog().Warn(fmt.Sprintf("MoA aggregator %s rejected adjacent same-role messages — merging them for this "+
			"destination for the rest of the session and retrying once: %.200s", moaSlotLabel(aggregator), err.Error()))
		aggMessages = retry
		if response, err = send(aggMessages); err != nil {
			return nil, err
		}
	}
	if trace != nil {
		if c.privacyMode != "" {
			trace.inputMessages = redactTraceMessages(aggMessages)
		} else {
			trace.inputMessages = aggMessages
		}
		trace.hasInputMessages = true
		trace.streamed = stream
		trace.output = nil
		if !stream {
			text := extractMoaText(response)
			trace.output = &text
		}
	}
	if m, ok := response.(map[string]any); ok && stream {
		if _, has := m["choices"]; has {
			return &moaOneChunkStream{chunk: completedResponseAsStreamChunk(m)}, nil
		}
	}
	return response, nil
}

// Turn scoped reference cache key for the preset fan out cadence
func (c *MoAChatCompletions) fanoutCacheKey(preset map[string]any, refMessages, referenceModels []map[string]any) *moaFanoutKey {
	fanoutMode := "user_turn"
	if pyTruthy(preset["fanout"]) {
		fanoutMode = pyrepr.Str(preset["fanout"])
	}
	fanoutMode = pystr.Lower(pystr.Strip(fanoutMode))
	everyN := 0
	if strings.HasPrefix(fanoutMode, "every_n:") {
		if n, ok := pyint.Parse(strings.SplitN(fanoutMode, ":", 2)[1]); ok {
			if n.IsInt64() && n.Int64() <= math.MaxInt32 {
				everyN = int(n.Int64())
			} else if n.Sign() > 0 {
				everyN = math.MaxInt32
			}
		}
		if everyN < 2 {
			fanoutMode = "per_iteration"
		}
	}
	sigMessages, turnPrefix := refMessages, refMessages
	if fanoutMode == "user_turn" || everyN >= 2 {
		for i := len(refMessages) - 1; i >= 0; i-- {
			if refMessages[i]["role"] == "user" && refMessages[i]["content"] != moaAdvisoryInstruction {
				turnPrefix = refMessages[:i+1]
				break
			}
		}
		if fanoutMode == "user_turn" {
			sigMessages = turnPrefix
		}
	}
	if everyN >= 2 {
		turnSig := hashMessages(turnPrefix)
		if turnSig != c.fanoutTurnSig {
			c.fanoutTurnSig = turnSig
			c.fanoutIterationCount = 0
			c.fanoutLastStateSig = ""
		}
		if stateSig := hashMessages(refMessages); stateSig != c.fanoutLastStateSig {
			c.fanoutLastStateSig = stateSig
			c.fanoutIterationCount++
		}
		if (c.fanoutIterationCount-1)%everyN != 0 && len(c.refCacheOutputs) > 0 {
			return c.refCacheKey
		}
	}
	labels := make([]string, len(referenceModels))
	for i, s := range referenceModels {
		labels[i] = moaSlotLabel(s)
	}
	return &moaFanoutKey{preset: c.PresetName, hash: hashMessages(sigMessages), labels: labels}
}

// Cache miss path running the advisors, folding their spend, staging the trace and emitting display events
func (c *MoAChatCompletions) runFanout(ctx context.Context, preset map[string]any, refMessages, referenceModels []map[string]any, aggregator map[string]any, aggregatorTemperature any, cacheKey *moaFanoutKey) ([]moaRefOutput, error) {
	var referenceTimeout *float64
	if raw := preset["reference_timeout"]; pyTruthy(raw) {
		f, err := pyint.Float(raw)
		if err != nil {
			return nil, err
		}
		referenceTimeout = &f
	}
	outputs := runReferencesParallel(ctx, referenceModels, refMessages, moaFanoutOpts{
		temperature: presetTemperature(preset, "reference_temperature"),
		progress: func(done, total int, label string) {
			c.emit("moa.progress", map[string]any{"refs_done": done, "refs_total": total, "label": label})
		},
		referenceTimeout: referenceTimeout, agent: c.agent, lateSink: c.recordLateReferenceAccounting,
	})
	interrupted := false
	for _, o := range outputs {
		if o.Text == moaInterruptedReferenceNote {
			interrupted = true
		}
	}
	if interrupted {
		c.refCacheKey, c.refCacheOutputs = nil, nil
	} else {
		c.refCacheKey, c.refCacheOutputs = cacheKey, append([]moaRefOutput{}, outputs...)
	}
	c.foldPendingAccounting(sumReferenceAccounting(outputs))
	privacyMode := c.privacyMode
	traceRefs := append([]moaRefOutput{}, outputs...)
	if privacyMode != "" {
		for i, o := range outputs {
			traceRefs[i] = moaRefOutput{Label: o.Label, Text: redactReferenceString(o.Text), Acct: redactTraceAccounting(o.Acct)}
		}
	}
	c.pendingTrace = &moaPendingTrace{preset: c.PresetName, referenceOutputs: traceRefs, aggregatorSlot: aggregator,
		aggregatorTemperature: aggregatorTemperature}
	func() {
		defer func() {
			if r := recover(); r != nil {
				moaLoopLog().Debug(fmt.Sprintf("MoA reference metrics render failed: %v", r))
				c.lastReferenceMetrics = nil
			}
		}()
		metrics := make([]any, len(traceRefs))
		for i, r := range traceRefs {
			var acct any
			if r.Acct != nil {
				acct = r.Acct
			}
			metrics[i] = SlotMetrics(acct, r.Label, r.Text)
		}
		c.lastReferenceMetrics = metrics
	}()
	count := len(outputs)
	for i, o := range outputs {
		text := o.Text
		if privacyMode != "" {
			text = redactReferenceString(text)
		}
		c.emit("moa.reference", map[string]any{"index": i + 1, "count": count, "label": o.Label, "text": text})
	}
	if count > 0 {
		aggLabel := moaSlotLabel(aggregator)
		c.emit("moa.phase", map[string]any{"phase": "aggregator", "refs_done": count, "refs_total": count, "aggregator": aggLabel})
		c.emit("moa.aggregating", map[string]any{"aggregator": aggLabel, "ref_count": count})
	}
	return outputs, nil
}

// Reference block attached to the aggregator prompt, false when there is nothing to attach
func (c *MoAChatCompletions) buildGuidance(outputs []moaRefOutput, aggregator map[string]any, policy string, stale bool) (string, bool) {
	aggRefs, degraded, allFailed := guidanceInputs(outputs, c.privacyMode == "full", policy)
	header := "[Mixture of Agents reference context]\n" + "Preset: " + c.PresetName + "\n" + "Aggregator/acting model: " + moaSlotLabel(aggregator) + "\n"
	if allFailed {
		moaLoopLog().Warn(fmt.Sprintf("MoA: all %d reference(s) failed — acting aggregator-alone without reference guidance", len(outputs)))
		if degraded != "" {
			return header + "\n" + "All reference models failed this turn — no advisory guidance is available. Act on your own judgment.\n\n" + degraded, true
		}
		return "", false
	}
	if len(aggRefs) > 0 || degraded != "" {
		labels := make([]string, len(aggRefs))
		for i, r := range aggRefs {
			labels[i] = r.Label
		}
		stalePart := ""
		if stale {
			stalePart = moaStaleGuidanceNote
		}
		return header + "References: " + strings.Join(labels, ", ") + "\n\n" +
			"Use the reference responses below as private context. You are the aggregator and acting model: " +
			"answer the user directly or call tools as needed.\n" + stalePart + "\n" + joinReferenceOutputs(aggRefs, degraded), true
	}
	return "", false
}

// Chat completions create where the aggregator acts on the transcript with the advisors' guidance attached
func (c *MoAChatCompletions) Create(ctx context.Context, apiKwargs map[string]any) (any, error) {
	kwargs := moaCopyRow(apiKwargs)
	if raw, ok := kwargs["_moa_prepared_request"]; ok {
		delete(kwargs, "_moa_prepared_request")
		if raw != nil {
			prepared, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("TypeError: _moa_prepared_request must be a dict")
			}
			return c.callPreparedAggregator(ctx, prepared, kwargs)
		}
	}
	prepareOnly := pyTruthy(kwargs["_moa_prepare_only"])
	delete(kwargs, "_moa_prepare_only")
	preset, moaRaw, err := resolvePresetCached(ctx, c.PresetName)
	if err != nil {
		return nil, err
	}
	c.privacyMode = moaPrivacyMode(moaRaw)
	messages := append([]map[string]any{}, mapRows(kwargs["messages"])...)
	var referenceModels []map[string]any
	if enabled, present := preset["enabled"]; !present || pyTruthy(enabled) {
		referenceModels = enabledSlots(preset["reference_models"])
	}
	aggregator, _ := preset["aggregator"].(map[string]any)
	if !pyTruthy(preset["aggregator"]) || aggregator == nil {
		aggregator = map[string]any{}
	}
	c.lastAggregatorSlot = nil
	if len(aggregator) > 0 {
		c.lastAggregatorSlot = moaCopyRow(aggregator)
	}
	var aggregatorTemperature any
	if t := presetTemperature(preset, "aggregator_temperature"); t != nil {
		aggregatorTemperature = *t
	} else if kwargs["temperature"] != nil {
		aggregatorTemperature = kwargs["temperature"]
	}
	refMessages := referenceMessages(messages)
	cacheKey := c.fanoutCacheKey(preset, refMessages, referenceModels)
	cacheHit := cacheKey.equal(c.refCacheKey) && len(c.refCacheOutputs) > 0
	var outputs []moaRefOutput
	if cacheHit {
		outputs = append([]moaRefOutput{}, c.refCacheOutputs...)
		c.pendingTrace = nil
	} else if outputs, err = c.runFanout(ctx, preset, refMessages, referenceModels, aggregator, aggregatorTemperature, cacheKey); err != nil {
		return nil, err
	}
	aggMessages := make([]map[string]any, len(messages))
	for i, m := range messages {
		aggMessages[i] = moaCopyRow(m)
	}
	policy := "loud"
	if pyTruthy(preset["degraded_reference_policy"]) {
		policy = pyrepr.Str(preset["degraded_reference_policy"])
	}
	var guidance any
	if text, ok := c.buildGuidance(outputs, aggregator, policy, cacheHit && toolActivitySinceLastUser(messages)); ok && text != "" {
		guidance = text
		aggMessages = attachReferenceGuidance(aggMessages, text)
	}
	prepared := map[string]any{"messages": aggMessages, "guidance": guidance, "aggregator": aggregator,
		"aggregator_temperature": aggregatorTemperature}
	if prepareOnly {
		return prepared, nil
	}
	return c.callPreparedAggregator(ctx, prepared, kwargs)
}

// MoAClient is the OpenAI client shaped wrapper whose chat completions is the MoA facade
type MoAClient struct {
	ctx  context.Context
	Chat struct{ Completions *MoAChatCompletions }
}

// NewMoAClient wraps a fresh facade, the context bound for calls whose interface carries none
func NewMoAClient(ctx context.Context, presetName string, referenceCallback func(event string, kwargs map[string]any), a *AIAgent) *MoAClient {
	c := &MoAClient{ctx: context.WithoutCancel(ctx)}
	c.Chat.Completions = NewMoAChatCompletions(presetName, referenceCallback, a)
	return c
}

// CreateChatCompletion forwards a chat completions create to the facade
func (c *MoAClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.Chat.Completions.Create(ctx, kwargs)
}

// PrepareRequest runs the fan out under the bound context and returns the prepared aggregator request
func (c *MoAClient) PrepareRequest(messages []map[string]any) (map[string]any, error) {
	return c.Chat.Completions.Prepare(c.ctx, messages)
}

// RebasePreparedRequest re attaches prepared guidance to a rebuilt transcript
func (c *MoAClient) RebasePreparedRequest(prepared map[string]any, messages []map[string]any) map[string]any {
	return c.Chat.Completions.RebasePreparedRequest(prepared, messages)
}

// ConsumeReferenceUsage pops the pending fan out usage and its dollar cost, nil cost when none priced
func (c *MoAClient) ConsumeReferenceUsage() (*provider.CanonicalUsage, any, error) {
	usage, cost := c.Chat.Completions.consumeReferenceUsage()
	if cost == nil {
		return &usage, nil, nil
	}
	f, _ := cost.Float64()
	return &usage, f, nil
}

// LastAggregatorSlot is the real aggregator slot of the latest create, nil when unset
func (c *MoAClient) LastAggregatorSlot() map[string]any {
	if c.Chat.Completions == nil {
		return nil
	}
	return c.Chat.Completions.lastAggregatorSlot
}

// ConsumeAndSaveTrace flushes the pending turn trace under the bound context, never failing
func (c *MoAClient) ConsumeAndSaveTrace(sessionID string, aggregatorOutputFallback *string) error {
	c.Chat.Completions.consumeAndSaveTrace(c.ctx, sessionID, aggregatorOutputFallback)
	return nil
}

// LastReferenceMetrics is the per advisor metrics list of the latest fan out, nil when none
func (c *MoAClient) LastReferenceMetrics() any {
	if m := c.Chat.Completions.lastReferenceMetrics; m != nil {
		return m
	}
	return nil
}

// Relay spec of one display event as primary kwarg, secondary kwarg and callback to emit kwarg map
type moaRelaySpec struct {
	primary   string
	secondary string
	extra     map[string]string
}

var moaRelayEvents = map[string]moaRelaySpec{
	"moa.reference":   {"label", "text", map[string]string{"moa_index": "index", "moa_count": "count"}},
	"moa.progress":    {"label", "", map[string]string{"moa_refs_done": "refs_done", "moa_refs_total": "refs_total"}},
	"moa.phase":       {"aggregator", "", map[string]string{"moa_phase": "phase", "moa_refs_done": "refs_done", "moa_refs_total": "refs_total"}},
	"moa.aggregating": {"aggregator", "", map[string]string{"moa_ref_count": "ref_count"}},
}

// Single construction point of the MoA client with the display relay reading the agent progress callback at emit time
func buildMoaFacade(ctx context.Context, a *AIAgent, presetName any) *MoAClient {
	relay := func(event string, kwargs map[string]any) {
		spec, ok := moaRelayEvents[event]
		if a == nil || a.toolProgressCallback == nil || !ok {
			return
		}
		defer func() { _ = recover() }()
		primary := moaStr(kwargs[spec.primary])
		var secondary any
		if spec.secondary != "" {
			secondary = moaStr(kwargs[spec.secondary])
		}
		extra := make(map[string]any, len(spec.extra))
		for out, src := range spec.extra {
			extra[out] = kwargs[src]
		}
		switch cb := progressFn(a.toolProgressCallback).(type) {
		case func(event, name, preview string, args map[string]any, extra map[string]any):
			preview, _ := secondary.(string)
			cb(event, primary, preview, nil, extra)
		case func(event, name string, preview any, args any, extra map[string]any):
			cb(event, primary, secondary, nil, extra)
		}
	}
	resolved := presetName
	if resolved == nil && a != nil && a.provider == "moa" {
		resolved = a.model
	}
	name := "default"
	if pyTruthy(resolved) {
		name = pyrepr.Str(resolved)
	}
	func() {
		defer func() {
			if recover() != nil {
				name = "default"
			}
		}()
		cfg, err := moaLoadConfig(ctx)
		if err != nil {
			name = "default"
			return
		}
		raw := cfg["moa"]
		if !pyTruthy(raw) {
			raw = map[string]any{}
		}
		normalized := provider.NormalizeMoAConfig(raw)
		presets, _ := moaMapOf(normalized["presets"])
		if _, ok := presets[name]; !ok {
			name = "default"
			if pyTruthy(normalized["default_preset"]) {
				name = pyrepr.Str(normalized["default_preset"])
			}
		}
	}()
	return NewMoAClient(ctx, name, relay, a)
}

// Pins the virtual moa runtime fields on the agent and installs the facade
func bindMoaRuntime(ctx context.Context, a *AIAgent, presetName string, apiKey any) error {
	if presetName == "" {
		presetName = "default"
	}
	a.model = presetName
	a.provider, a.requestedProvider = "moa", "moa"
	a.apiMode = "chat_completions"
	a.apiKey = moaVirtualAPIKey
	if pyTruthy(apiKey) {
		a.apiKey = pyrepr.Str(apiKey)
	}
	a.baseURL = moaVirtualBaseURL
	a.clientKwargs = map[string]any{}
	a.client = buildMoaFacade(ctx, a, a.model)
	return nil
}

// Real aggregator slot of the agent's MoA client, nil off the MoA path
func lastAggregatorSlot(a *AIAgent) map[string]any {
	if c, ok := a.client.(moaAggregatorSlotClient); ok {
		return c.LastAggregatorSlot()
	}
	return nil
}
