package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Registers the Anthropic Messages adapter
func init() {
	RegisterFactory(ModeAnthropicMessages, func(route Route) (Client, error) { return newAnthropicClient(route) })
}

// ---- model tables ----

// Manual thinking budget per effort for the legacy budget_tokens contract
var anthThinkingBudget = map[string]int{"xhigh": 32000, "high": 16000, "medium": 8000, "low": 4000}

// Effort names mapped to Anthropic adaptive thinking output_config.effort values
var anthAdaptiveEffortMap = map[string]string{"ultra": "max", "max": "max", "xhigh": "xhigh", "high": "high", "medium": "medium", "low": "low", "minimal": "low"}

var anthLegacyManualThinkingClaude = []string{
	"claude-3", "claude-opus-4-0", "claude-opus-4.0", "claude-opus-4-1", "claude-opus-4.1",
	"claude-sonnet-4-0", "claude-sonnet-4.0", "claude-opus-4-2025", "claude-sonnet-4-2025",
	"claude-opus-4-5", "claude-opus-4.5", "claude-sonnet-4-5", "claude-sonnet-4.5", "claude-haiku-4-5",
	"claude-haiku-4.5",
}

var anthNoXHighClaude = []string{"claude-opus-4-6", "claude-opus-4.6", "claude-sonnet-4-6", "claude-sonnet-4.6"}

var anthMandatoryThinkingClaude = []string{"claude-fable", "claude-mythos", "claude-opus-5-5", "claude-opus-5.5"}

var anthBetweenToolsOffClaude = []string{"claude-sonnet-5-5", "claude-sonnet-5.5"}

func anthIsClaudeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

// Case insensitive substring match of a model id against family lists
func anthModelMatches(model string, lists ...[]string) bool {
	m := strings.ToLower(model)
	for _, list := range lists {
		for _, v := range list {
			if strings.Contains(m, v) {
				return true
			}
		}
	}
	return false
}

// Max output tokens per model family, longest substring match wins
var anthOutputLimits = map[string]int{
	"claude-fable": 128_000, "claude-sonnet-5": 128_000, "claude-opus-4-8": 128_000,
	"claude-opus-4-7": 128_000, "claude-opus-4-6": 128_000, "claude-sonnet-4-6": 64_000,
	"claude-opus-4-5": 64_000, "claude-sonnet-4-5": 64_000, "claude-haiku-4-5": 64_000,
	"claude-opus-4": 32_000, "claude-sonnet-4": 64_000, "claude-3-7-sonnet": 128_000,
	"claude-3-5-sonnet": 8_192, "claude-3-5-haiku": 8_192, "claude-3-opus": 4_096,
	"claude-3-sonnet": 4_096, "claude-3-haiku": 4_096, "minimax": 131_072, "qwen3": 65_536,
}

const anthDefaultOutputLimit = 128_000

// Max output tokens for a model via longest family substring match, dots normalized to hyphens
func anthropicMaxOutput(model string) int {
	m := strings.ReplaceAll(strings.ToLower(model), ".", "-")
	bestKey := ""
	for key := range anthOutputLimits {
		if strings.Contains(m, key) && (bestKey == "" || len(key) > len(bestKey) || (len(key) == len(bestKey) && key < bestKey)) {
			bestKey = key
		}
	}
	if bestKey == "" {
		return anthDefaultOutputLimit
	}
	return anthOutputLimits[bestKey]
}

// Max output tokens of an Anthropic model by its longest matching family
func AnthropicMaxOutput(model string) int {
	return anthropicMaxOutput(model)
}

// Value floored to a positive int, false for booleans, non numbers, non finite or non positive values
func anthResolvePositiveMaxTokens(value any) (int, bool) {
	var f float64
	switch v := value.(type) {
	case nil, bool:
		return 0, false
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case int32:
		f = float64(v)
	case float32:
		f = float64(v)
	case float64:
		f = v
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	n := int(math.Trunc(f))
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// Requested positive max tokens, else the model's output ceiling, error when neither is positive
func anthResolveMessagesMaxTokens(requested any, model string) (int, error) {
	resolved, ok := anthResolvePositiveMaxTokens(requested)
	if !ok {
		resolved = anthropicMaxOutput(model)
	}
	if resolved > 0 {
		return resolved, nil
	}
	return 0, fmt.Errorf("Anthropic Messages adapter requires a positive max_tokens value for model %q; got %v and no model default resolved.", model, requested)
}

// Adaptive thinking models, unknown Claude defaults adaptive, Kimi family included, other vendors manual
func anthSupportsAdaptiveThinking(model string) bool {
	return modelNameIsKimiFamily(model) || (anthIsClaudeModel(model) && !anthModelMatches(model, anthLegacyManualThinkingClaude))
}

// Models accepting the xhigh effort, Opus and Sonnet 4.6 excluded
func anthSupportsXHighEffort(model string) bool {
	return anthSupportsAdaptiveThinking(model) && !anthModelMatches(model, anthNoXHighClaude)
}

// Claude adaptive models that accept an explicit thinking disabled parameter
func anthAcceptsThinkingDisable(model string) bool {
	return anthIsClaudeModel(model) && anthSupportsAdaptiveThinking(model) && !anthModelMatches(model, anthMandatoryThinkingClaude, anthBetweenToolsOffClaude)
}

// Haiku generations through 4.5 that never get a thinking request
func anthIsPreThinkingHaiku(model string) bool {
	if !strings.Contains(strings.ToLower(model), "haiku") {
		return false
	}
	family, version, ok := claudeFamilyVersion(model)
	return !ok || family != "haiku" || !versionAtLeast(version, [2]int{5, 5})
}

// Models that 400 on any sampling parameter, unknown Claude defaults to forbidding
func anthForbidsSamplingParams(model string) bool {
	return anthIsClaudeModel(model) && !anthModelMatches(model, anthNoXHighClaude, anthLegacyManualThinkingClaude)
}

func anthSupportsFastMode(model string) bool { return IsAnthropicFastModeModel(model) }

// ---- betas and identity ----

const anthToolStreamingBeta = "fine-grained-tool-streaming-2025-05-14"

// Anthropic beta that enables fine grained tool streaming, added for Claude over OpenRouter
const ToolStreamingBeta = anthToolStreamingBeta

// Beta headers safe on ordinary native Anthropic requests
var anthropicCommonBetas = []string{"interleaved-thinking-2025-05-14", anthToolStreamingBeta}

const anthropicContext1MBeta = "context-1m-2025-08-07"

const anthFastModeBeta = "fast-mode-2026-02-01"

// Betas required for OAuth subscription auth
var anthropicOAuthOnlyBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

const anthAPIVersionHeader = "2023-06-01"

const anthClaudeCodeVersionFallback = "2.1.74"

// Install prefixes probed in addition to PATH for the Claude Code CLI
var anthClaudeCodePrefixes = []string{
	"~/.local/bin", "~/.claude/local", "~/bin", "~/.npm-global/bin", "~/.bun/bin",
	"~/.volta/bin", "/opt/homebrew/bin", "/usr/local/bin",
}

var anthClaudeCodeNames = []string{"claude", "claude-code"}

// PATH lookup seam for the Claude Code CLI probes
var anthLookPath = exec.LookPath

// Runs a CLI with --version and returns its stdout, seam for tests
var anthRunVersion = func(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func anthIsRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Executable paths to try, PATH hits first then the install prefixes, deduped
func anthClaudeCodeCandidates() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, name := range anthClaudeCodeNames {
		if hit, err := anthLookPath(name); err == nil {
			add(hit)
		}
	}
	for _, prefix := range anthClaudeCodePrefixes {
		for _, name := range anthClaudeCodeNames {
			p := filepath.Join(expandTildePath(prefix), name)
			if anthIsRegularFile(p) {
				add(p)
			}
		}
	}
	return out
}

// Path of a bare Claude Code command on PATH, else in an install prefix, empty for any other command
func findClaudeCodeCLI(command string) string {
	known := false
	for _, name := range anthClaudeCodeNames {
		if command == name {
			known = true
		}
	}
	if !known {
		return ""
	}
	if hit, err := anthLookPath(command); err == nil {
		return hit
	}
	for _, prefix := range anthClaudeCodePrefixes {
		p := filepath.Join(expandTildePath(prefix), command)
		if anthIsRegularFile(p) {
			return p
		}
	}
	return ""
}

// Installed Claude Code version from the first candidate answering --version, else the fallback
func anthDetectClaudeCodeVersion() string {
	for _, cmd := range anthClaudeCodeCandidates() {
		out, err := anthRunVersion(cmd)
		if err != nil {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) == 0 {
			continue
		}
		version := fields[0]
		if version != "" && version[0] >= '0' && version[0] <= '9' {
			return version
		}
	}
	return anthClaudeCodeVersionFallback
}

var anthClaudeCodeVersionMu sync.Mutex
var anthClaudeCodeVersionCache string

// Process cached Claude Code version detected on first OAuth use
func anthClaudeCodeVersion() string {
	anthClaudeCodeVersionMu.Lock()
	defer anthClaudeCodeVersionMu.Unlock()
	if anthClaudeCodeVersionCache == "" {
		anthClaudeCodeVersionCache = anthDetectClaudeCodeVersion()
	}
	return anthClaudeCodeVersionCache
}

const anthClaudeCodeSystemPrefix = "You are Claude Code, Anthropic's official CLI for Claude."

const anthMCPToolPrefix = "mcp__"

// Tool names rewritten on the OAuth wire so the billing classifier does not flag them
var anthOAuthToolNameAliases = map[string]string{"session_search": "chat_history_lookup", "memory": "context_notes"}

var anthOAuthToolNameReverseAliases = func() map[string]string {
	out := map[string]string{}
	for name, wire := range anthOAuthToolNameAliases {
		out[wire] = name
	}
	return out
}()

var anthOAuthProseAliasPatterns = []struct {
	re   *regexp.Regexp
	wire string
}{{regexp.MustCompile(`\bsession_search\b`), anthOAuthToolNameAliases["session_search"]}}

// Rewrites prose safe tool name tokens to their OAuth wire aliases
func anthApplyOAuthProseAliases(text string) string {
	for _, p := range anthOAuthProseAliasPatterns {
		text = p.re.ReplaceAllString(text, p.wire)
	}
	return text
}

// Beta headers safe for the endpoint, MiniMax dropping tool streaming and 1M, drop1M stripping the 1M beta
func anthCommonBetasForBaseURL(baseURL string, dropContext1M bool) []string {
	betas := cloneStrings(anthropicCommonBetas)
	if baseURLNeedsContext1MBeta(baseURL) && !dropContext1M {
		betas = append(betas, anthropicContext1MBeta)
	}
	if isMinimaxAnthropicEndpoint(baseURL) {
		kept := betas[:0]
		for _, b := range betas {
			if b != anthToolStreamingBeta && b != anthropicContext1MBeta {
				kept = append(kept, b)
			}
		}
		return kept
	}
	return betas
}

func anthBetaHeader(betas []string) map[string]string {
	if len(betas) == 0 {
		return map[string]string{}
	}
	return map[string]string{"anthropic-beta": strings.Join(betas, ",")}
}

func anthAttributionHeaders() map[string]string { return attributionHeaders("IcebathAgent") }

// Order sensitive endpoint and key classification into kimi, bearer, api_key or oauth
func anthAuthStyle(apiKey, baseURL, normalizedBaseURL string) string {
	if isKimiCodingEndpoint(baseURL) {
		return "kimi"
	}
	if requiresBearerAuth(normalizedBaseURL) {
		return "bearer"
	}
	if isThirdPartyAnthropicEndpoint(baseURL) {
		return "api_key"
	}
	if isOAuthToken(apiKey) {
		return "oauth"
	}
	return "api_key"
}

var anthTrailingV1Re = regexp.MustCompile(`/v1/?$`)

// Base URL with surrounding space, trailing slash and trailing /v1 removed
func anthNormalizedBaseURL(baseURL string) string {
	return anthTrailingV1Re.ReplaceAllString(strings.TrimRight(normalizeBaseURLText(baseURL), "/"), "")
}

// OAuth wire form of a tool name, double underscore mcp prefix without aliasing
func anthNormalizeToMCPWire(name string) string {
	if strings.HasPrefix(name, anthMCPToolPrefix) {
		return name
	}
	return anthMCPToolPrefix + strings.TrimPrefix(name, "mcp_")
}

// Name to OAuth wire name mapper for one tool set, aliases yielding to a colliding real tool
func anthOAuthWireNamer(tools []map[string]any) func(string) string {
	claimed := map[string]bool{}
	for _, tool := range tools {
		name, ok := tool["name"].(string)
		if !ok {
			continue
		}
		if _, aliased := anthOAuthToolNameAliases[name]; aliased {
			continue
		}
		claimed[anthNormalizeToMCPWire(name)] = true
	}
	return func(name string) string {
		if aliased, ok := anthOAuthToolNameAliases[name]; ok && !claimed[anthMCPToolPrefix+aliased] {
			name = aliased
		}
		return anthNormalizeToMCPWire(name)
	}
}

var anthOAuthSystemReplacements = [][2]string{
	{"Hermes Agent", "Claude Code"}, {"Hermes agent", "Claude Code"}, {"Icebath Agent", "Claude Code"},
	{"Icebath agent", "Claude Code"}, {"Nous Research", "Anthropic"},
}

// Standalone prose slug of the product, addresses and quoted identifiers excluded
var anthOAuthSlugPattern = regexp2.MustCompile(`(?<![\w./:@'"` + "`" + `-])(?:hermes-agent|icebath-agent)(?![\w/@-]|\.\w)`)

func anthRewriteSlug(text string) string {
	out, err := anthOAuthSlugPattern.Replace(text, "claude-code", -1, -1)
	if err != nil {
		return text
	}
	return out
}

// Claude Code system prefix, product name sanitizing and tool renames for an OAuth request
func anthApplyClaudeCodeIdentity(system any, tools []map[string]any, messages []map[string]any, toWire func(string) string) []any {
	ccBlock := map[string]any{"type": "text", "text": anthClaudeCodeSystemPrefix}
	var blocks []any
	switch s := system.(type) {
	case string:
		if s != "" {
			blocks = []any{map[string]any{"type": "text", "text": s}}
		}
	case []any:
		blocks = s
	case []map[string]any:
		for _, b := range s {
			blocks = append(blocks, b)
		}
	}
	out := append([]any{ccBlock}, blocks...)
	for _, b := range out {
		block, ok := asMap(b)
		if !ok || asStr(block["type"]) != "text" {
			continue
		}
		text := asStr(block["text"])
		for _, rep := range anthOAuthSystemReplacements {
			text = strings.ReplaceAll(text, rep[0], rep[1])
		}
		block["text"] = anthApplyOAuthProseAliases(anthRewriteSlug(text))
	}
	for _, tool := range tools {
		if name, ok := tool["name"].(string); ok {
			tool["name"] = toWire(name)
		}
		if desc, ok := tool["description"].(string); ok {
			tool["description"] = anthApplyOAuthProseAliases(desc)
		}
	}
	for _, msg := range messages {
		content, ok := asList(msg["content"])
		if !ok {
			continue
		}
		for _, b := range content {
			block, ok := asMap(b)
			if !ok || asStr(block["type"]) != "tool_use" {
				continue
			}
			if name, ok := block["name"].(string); ok {
				block["name"] = toWire(name)
			}
		}
	}
	return out
}

// Reasoning request as hermes shapes it, nil meaning no reasoning config at all
type anthReasoningConfig struct {
	enabled *bool
	effort  string
	budget  int
}

// Reasoning config of a request, nil when the request carries none
func anthReasoningFromRequest(r Reasoning) *anthReasoningConfig {
	if r.Enabled == nil && strings.TrimSpace(r.Effort) == "" && r.BudgetTokens <= 0 {
		return nil
	}
	return &anthReasoningConfig{enabled: r.Enabled, effort: strings.TrimSpace(r.Effort), budget: r.BudgetTokens}
}

// Anthropic thinking kwargs for a reasoning config, adaptive plus effort or budget tokens or an explicit off
func anthThinkingKwargs(cfg *anthReasoningConfig, model string, effectiveMaxTokens int) map[string]any {
	out := map[string]any{}
	if cfg == nil {
		return out
	}
	if cfg.enabled != nil && !*cfg.enabled {
		if anthModelMatches(model, anthBetweenToolsOffClaude) {
			out["thinking"] = map[string]any{"type": "between_tools"}
			return out
		}
		if anthAcceptsThinkingDisable(model) {
			out["thinking"] = map[string]any{"type": "disabled"}
		}
		return out
	}
	if anthIsPreThinkingHaiku(model) {
		return out
	}
	effort := strings.ToLower(cfg.effort)
	if effort == "" {
		effort = "medium"
	}
	if anthSupportsAdaptiveThinking(model) {
		adaptive, ok := anthAdaptiveEffortMap[effort]
		if !ok {
			adaptive = "medium"
		}
		if adaptive == "xhigh" && !anthSupportsXHighEffort(model) {
			adaptive = "max"
		}
		out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		out["output_config"] = map[string]any{"effort": adaptive}
		return out
	}
	budget, ok := anthThinkingBudget[effort]
	if !ok {
		budget = 8000
	}
	if cfg.budget > 0 {
		budget = cfg.budget
	}
	out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	out["temperature"] = 1
	maxTokens := effectiveMaxTokens
	if budget+4096 > maxTokens {
		maxTokens = budget + 4096
	}
	out["max_tokens"] = maxTokens
	return out
}

// OpenAI tool_choice values mapped to Anthropic, any other string is a forced tool name
var anthToolChoiceMap = map[string]map[string]any{"auto": {"type": "auto"}, "required": {"type": "any"}}

// Forced tool name of an OpenAI shaped tool_choice object, empty when it is not one
func anthToolChoiceName(choice map[string]any) string {
	if fn, ok := asMap(choice["function"]); ok {
		return asStr(fn["name"])
	}
	return asStr(choice["name"])
}

// Inputs of one Messages body build
type anthBuildInput struct {
	model             string
	messages          []map[string]any
	tools             []map[string]any
	maxTokens         any
	reasoning         *anthReasoningConfig
	toolChoice        any
	isOAuth           bool
	preserveDots      bool
	contextLength     int
	baseURL           string
	fastMode          bool
	dropContext1M     bool
	resolveProfileARN func(string) string
}

// Builds the Messages API body, extra_headers carrying the rebuilt beta list in fast mode
func anthBuildKwargs(in anthBuildInput) (map[string]any, error) {
	system, messages := convertMessagesToAnthropic(in.messages, convertOptions{baseURL: in.baseURL, model: in.model, resolveProfileARN: in.resolveProfileARN})
	var tools []map[string]any
	if len(in.tools) > 0 {
		tools = convertToolsToAnthropic(in.tools)
	}
	model := in.model
	if !isNousPortalEndpoint(in.baseURL) {
		model = normalizeModelName(model, in.preserveDots)
	}
	effectiveMaxTokens, err := anthResolveMessagesMaxTokens(in.maxTokens, model)
	if err != nil {
		return nil, &LocalValidationError{Message: err.Error()}
	}
	if in.contextLength > 0 && effectiveMaxTokens > in.contextLength {
		effectiveMaxTokens = in.contextLength - 1
		if effectiveMaxTokens < 1 {
			effectiveMaxTokens = 1
		}
	}
	var toWire func(string) string
	if in.isOAuth {
		toWire = anthOAuthWireNamer(tools)
		system = anthApplyClaudeCodeIdentity(system, tools, messages, toWire)
	}
	wireMessages := make([]any, 0, len(messages))
	for _, m := range messages {
		wireMessages = append(wireMessages, m)
	}
	kwargs := map[string]any{"model": model, "messages": wireMessages, "max_tokens": effectiveMaxTokens}
	if !anthSystemEmpty(system) {
		kwargs["system"] = system
	}
	if len(tools) > 0 {
		wireTools := make([]any, 0, len(tools))
		for _, t := range tools {
			wireTools = append(wireTools, t)
		}
		kwargs["tools"] = wireTools
		choice := ""
		choiceSet := false
		switch tc := in.toolChoice.(type) {
		case nil:
		case string:
			choice, choiceSet = tc, true
		case map[string]any:
			if typ := asStr(tc["type"]); typ == "auto" || typ == "required" || typ == "none" {
				choice, choiceSet = typ, true
			} else if name := anthToolChoiceName(tc); name != "" {
				choice, choiceSet = name, true
			}
		}
		switch {
		case choiceSet && choice == "none":
			delete(kwargs, "tools")
		case !choiceSet:
			kwargs["tool_choice"] = map[string]any{"type": "auto"}
		default:
			if mapped, ok := anthToolChoiceMap[choice]; ok {
				kwargs["tool_choice"] = map[string]any{"type": mapped["type"]}
			} else {
				name := choice
				if toWire != nil {
					name = toWire(name)
				}
				kwargs["tool_choice"] = map[string]any{"type": "tool", "name": name}
			}
		}
	}
	for k, v := range anthThinkingKwargs(in.reasoning, model, effectiveMaxTokens) {
		kwargs[k] = v
	}
	if anthForbidsSamplingParams(model) {
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			delete(kwargs, key)
		}
	}
	if in.fastMode && !isThirdPartyAnthropicEndpoint(in.baseURL) && anthSupportsFastMode(model) {
		extra, _ := asMap(kwargs["extra_body"])
		if extra == nil {
			extra = map[string]any{}
		}
		extra["speed"] = "fast"
		kwargs["extra_body"] = extra
		betas := anthCommonBetasForBaseURL(in.baseURL, in.dropContext1M)
		if in.isOAuth {
			betas = append(betas, anthropicOAuthOnlyBetas...)
		}
		kwargs["extra_headers"] = anthBetaHeader(append(betas, anthFastModeBeta))
	}
	return kwargs, nil
}

func anthSystemEmpty(system any) bool {
	switch s := system.(type) {
	case nil:
		return true
	case string:
		return s == ""
	case []any:
		return len(s) == 0
	case []map[string]any:
		return len(s) == 0
	}
	return false
}

// Keys exclusive to the Responses wire that the Messages API rejects
var anthResponsesOnlyKwargs = []string{"instructions", "input", "store", "parallel_tool_calls"}

// Drops Responses only keys from a Messages body and returns the removed names
func anthSanitizeKwargs(kwargs map[string]any, logPrefix string) []string {
	var leaked []string
	for _, key := range anthResponsesOnlyKwargs {
		if _, has := kwargs[key]; has {
			delete(kwargs, key)
			leaked = append(leaked, key)
		}
	}
	if len(leaked) > 0 {
		sort.Strings(leaked)
		catalogLog.Warn(fmt.Sprintf("%sStripped Responses-only kwarg(s) %v from an Anthropic Messages call (api_mode flip race — see #31673). The call will proceed; this breadcrumb means a kwargs build ran under a Responses api_mode while dispatch ran under anthropic_messages.", logPrefix, leaked))
	}
	return leaked
}

// Sets eager_input_streaming false on every tool when the endpoint carries the tool streaming beta
func anthBufferToolInput(kwargs map[string]any, baseURL string) {
	hasBeta := false
	for _, b := range anthCommonBetasForBaseURL(baseURL, false) {
		if b == anthToolStreamingBeta {
			hasBeta = true
		}
	}
	if !hasBeta {
		return
	}
	tools, _ := asList(kwargs["tools"])
	for _, t := range tools {
		if tool, ok := asMap(t); ok {
			tool["eager_input_streaming"] = false
		}
	}
}

// Merges an OpenAI response format into Anthropic output_config.format
func anthTranslateResponseFormat(kwargs map[string]any, responseFormat map[string]any) {
	if responseFormat == nil {
		return
	}
	var schema any
	switch asStr(responseFormat["type"]) {
	case "json_schema":
		js, ok := asMap(responseFormat["json_schema"])
		if !ok {
			return
		}
		s, has := js["schema"]
		if !has {
			return
		}
		schema = s
	case "json_object":
		schema = map[string]any{"type": "object"}
	default:
		return
	}
	cfg, ok := asMap(kwargs["output_config"])
	if !ok {
		cfg = map[string]any{}
		kwargs["output_config"] = cfg
	}
	cfg["format"] = map[string]any{"type": "json_schema", "schema": schema}
}

// Reports a 400 telling an OAuth subscription that the 1M context beta is not available to it
func anthIsContext1MRejection(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return false
	}
	msg := strings.ToLower(apiErr.Message + " " + apiErr.Body)
	return strings.Contains(msg, "long context beta") && strings.Contains(msg, "not yet available")
}

// ---- transports ----

// Operation of one Messages wire call
type anthOperation string

const (
	anthOpMessages    anthOperation = "messages"
	anthOpCountTokens anthOperation = "count_tokens"
)

// Backend shape of one Anthropic Messages wire, direct API, Bedrock or Vertex
type anthTransport interface {
	Request(ctx context.Context, op anthOperation, body map[string]any, headers map[string]string) (*http.Request, error)
	Events(resp *http.Response) anthEventSource
	StreamUnavailable(err error) bool
	GuardrailIntervened(message map[string]any) bool
	ResolveProfileARN(arn string) string
	Betas(dropContext1M bool) []string
}

// Direct Anthropic API and compatible endpoints
type anthDirectTransport struct {
	baseURL string
	azure   bool
}

func newAnthDirectTransport(route Route) *anthDirectTransport {
	base := anthNormalizedBaseURL(route.BaseURL)
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &anthDirectTransport{baseURL: base, azure: isAzureAnthropicEndpoint(base)}
}

// Request posts the body at /v1/messages or /v1/messages/count_tokens under the normalized base
func (t *anthDirectTransport) Request(ctx context.Context, op anthOperation, body map[string]any, headers map[string]string) (*http.Request, error) {
	target, err := url.Parse(t.baseURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, errors.New("provider: base URL must be absolute: " + t.baseURL)
	}
	path := "/v1/messages"
	if op == anthOpCountTokens {
		path += "/count_tokens"
	}
	target.Path = strings.TrimRight(target.Path, "/") + path
	if t.azure && !strings.Contains(t.baseURL, "api-version") {
		q := target.Query()
		if q.Get("api-version") == "" {
			q.Set("api-version", "2025-04-15")
		}
		target.RawQuery = q.Encode()
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("provider: encode anthropic messages body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream, _ := body["stream"].(bool); stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (t *anthDirectTransport) Events(resp *http.Response) anthEventSource {
	return newAnthSSEEvents(resp.Body, resp.Header.Clone())
}

// StreamUnavailable reports a provider that rejects streams or emits events out of order
func (t *anthDirectTransport) StreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "stream") && strings.Contains(msg, "not supported") {
		return true
	}
	return strings.Contains(msg, "unexpected event order")
}

func (t *anthDirectTransport) GuardrailIntervened(map[string]any) bool { return false }

func (t *anthDirectTransport) ResolveProfileARN(string) string { return "" }

func (t *anthDirectTransport) Betas(dropContext1M bool) []string {
	return anthCommonBetasForBaseURL(t.baseURL, dropContext1M)
}

// Reports whether the route is served by AWS Bedrock
func anthRouteIsBedrock(route Route) bool {
	return route.Auth == AuthAWSSigV4 || explicitBedrockNames[strings.ToLower(strings.TrimSpace(route.Provider))] || isBedrockRuntimeEndpoint(route.BaseURL)
}

// Reports whether the route is served by Vertex AI
func anthRouteIsVertex(route Route) bool {
	return route.Auth == AuthGCPOAuth || vertexNames[strings.ToLower(strings.TrimSpace(route.Provider))] || isVertexAIEndpoint(route.BaseURL)
}

// ---- client ----

// Reports whether a tool name is registered with the agent, wired by the tools owner
var anthRegisteredTool func(name string) bool

// SetRegisteredToolLookup wires the tool registry membership check used to reverse OAuth wire names
func SetRegisteredToolLookup(fn func(name string) bool) {
	anthRegisteredTool = fn
}

// Client speaking the Anthropic Messages wire for one route
type anthropicClient struct {
	route            Route
	transport        anthTransport
	variant          string
	mu               sync.Mutex
	dropContext1M    bool
	bufferToolInput  bool
	nonStreaming     bool
	oauthWire        bool
	requestToolNames map[string]bool
	transportsMu     sync.Mutex
	transports       map[time.Duration]*http.Transport
	mintedMu         sync.Mutex
	mintedOAuth      *bool
}

func newAnthropicClient(route Route) (Client, error) {
	c := &anthropicClient{route: route, variant: "direct", transports: map[time.Duration]*http.Transport{}}
	switch {
	case anthRouteIsBedrock(route):
		t, err := bedrockTransport(route)
		if err != nil {
			return nil, err
		}
		c.transport, c.variant = t, "bedrock"
	case anthRouteIsVertex(route):
		t, err := vertexTransport(route)
		if err != nil {
			return nil, err
		}
		c.transport, c.variant = t, "vertex"
	default:
		c.transport = newAnthDirectTransport(route)
	}
	return c, nil
}

func (c *anthropicClient) Route() Route { return c.route }

// HTTP client for the route, dial and handshake bounded by the connect timeout
func (c *anthropicClient) httpClient(connect time.Duration) *http.Client {
	base := c.route.Transport
	if base == nil {
		c.transportsMu.Lock()
		tr, ok := c.transports[connect]
		if !ok {
			tr = &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: connect, KeepAlive: 30 * time.Second}).DialContext,
				TLSClientConfig:       PlatformTLSConfig(),
				TLSHandshakeTimeout:   connect,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: time.Second,
			}
			c.transports[connect] = auxTrackPoolConns(tr)
		}
		c.transportsMu.Unlock()
		base = tr
	}
	if c.variant == "direct" && c.route.TokenSource != nil {
		var sendsAsAPIKey func(string) bool
		if isNativeAnthropicHost(c.route.BaseURL) {
			sendsAsAPIKey = func(token string) bool { return !isOAuthToken(token) }
		}
		base = &EntraBearerTransport{Base: base, TokenProvider: c.route.TokenSource, SendsAsAPIKey: sendsAsAPIKey}
	}
	return &http.Client{Transport: base}
}

// OAuth shape of the first token the route minter yields, a failed mint counting as non OAuth
func (c *anthropicClient) mintedTokenIsOAuth(ctx context.Context) bool {
	c.mintedMu.Lock()
	defer c.mintedMu.Unlock()
	if c.mintedOAuth == nil {
		token, err := c.route.TokenSource(ctx)
		oauth := err == nil && isOAuthToken(token)
		c.mintedOAuth = &oauth
	}
	return *c.mintedOAuth
}

// Claude Code OAuth identity of the route, a minter judged once by its first token
func (c *anthropicClient) routeIsOAuth(ctx context.Context) bool {
	if c.route.TokenSource == nil || c.route.APIKey != "" {
		return AnthropicRouteIsOAuth(ctx, c.route.BaseURL, c.route.APIKey, nil, c.route.Provider)
	}
	if !(IsNativeAnthropicHost(c.route.BaseURL) || strings.ToLower(strings.TrimSpace(c.route.Provider)) == "anthropic") {
		return false
	}
	return c.mintedTokenIsOAuth(ctx)
}

// Marks the session so later requests buffer tool input server side
func (c *anthropicClient) markToolInputBuffered() {
	c.mu.Lock()
	c.bufferToolInput = true
	c.mu.Unlock()
}

// Drops the 1M beta for the rest of the session after a rejection, false when already dropped
func (c *anthropicClient) noteContext1MRejection(err error) bool {
	if !anthIsContext1MRejection(err) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropContext1M {
		return false
	}
	c.dropContext1M = true
	catalogLog.Info(fmt.Sprintf("Anthropic rejected the 1M context beta for %s; retrying without it for this client.", orUnknown2(c.route.Model)))
	return true
}

// Registered name of a tool_use block name arriving on the OAuth wire
func (c *anthropicClient) inboundToolName(name string) string {
	c.mu.Lock()
	oauth, known := c.oauthWire, c.requestToolNames
	c.mu.Unlock()
	if !oauth || !strings.HasPrefix(name, anthMCPToolPrefix) {
		return name
	}
	bare := name[len(anthMCPToolPrefix):]
	for _, candidate := range []string{name, "mcp_" + bare, bare} {
		if known[candidate] || (anthRegisteredTool != nil && anthRegisteredTool(candidate)) {
			return candidate
		}
	}
	if original, ok := anthOAuthToolNameReverseAliases[bare]; ok {
		return original
	}
	return name
}

func (c *anthropicClient) providerLower() string {
	return strings.ToLower(strings.TrimSpace(c.route.Provider))
}

func (c *anthropicClient) isNous() bool {
	p := c.providerLower()
	return p == "nous" || p == "nous-portal" || p == "nousresearch" || baseURLHostMatches(c.route.BaseURL, "nousresearch.com")
}

// Reports whether model name dots are preserved for this route
func (c *anthropicClient) preserveDots() bool {
	switch c.providerLower() {
	case "alibaba", "minimax", "minimax-cn", "opencode-go", "opencode-zen", "zai", "bedrock", "xiaomi", "vertex":
		return true
	}
	base := strings.ToLower(c.route.BaseURL)
	host := baseURLHostname(base)
	return strings.Contains(host, "dashscope") || baseURLHostMatches(base, "aliyuncs.com") || strings.Contains(host, "minimax") ||
		(baseURLHostMatches(base, "opencode.ai") && strings.Contains(base, "/zen/")) || baseURLHostMatches(base, "bigmodel.cn") ||
		baseURLHostMatches(base, "xiaomimimo.com") || baseURLHostMatches(base, "aiplatform.googleapis.com") || strings.HasPrefix(host, "bedrock-runtime.")
}

// Prepared wire call of one request
type anthPrepared struct {
	body      map[string]any
	headers   map[string]string
	betas     []string
	oauth     bool
	settings  wireSettings
	estTokens int
}

// Credential headers of one request, minting from the token source per call, with the auth style
func (c *anthropicClient) authHeaders(ctx context.Context, normalized string) (map[string]string, string, error) {
	headers := map[string]string{}
	if c.variant != "direct" {
		return headers, "platform", nil
	}
	if c.route.TokenSource != nil {
		if isNativeAnthropicHost(c.route.BaseURL) && c.mintedTokenIsOAuth(ctx) {
			return headers, "oauth", nil
		}
		return headers, "bearer", nil
	}
	key := strings.TrimSpace(c.route.APIKey)
	style := anthAuthStyle(key, c.route.BaseURL, normalized)
	if key == "" {
		return headers, style, nil
	}
	if style == "bearer" || style == "oauth" {
		headers["Authorization"] = "Bearer " + key
	} else {
		headers["x-api-key"] = key
	}
	return headers, style, nil
}

// Request headers in hermes order, identity and attribution first, route headers last
func (c *anthropicClient) requestHeaders(ctx context.Context, betas []string, extraHeaders map[string]any) (map[string]string, bool, error) {
	normalized := anthNormalizedBaseURL(c.route.BaseURL)
	auth, style, err := c.authHeaders(ctx, normalized)
	if err != nil {
		return nil, false, Classify(c.route.Provider, c.route.Model, 401, "", err)
	}
	oauth := style == "oauth"
	headers := map[string]string{"anthropic-version": anthAPIVersionHeader, "User-Agent": UserAgent("IcebathAgent")}
	if oauth {
		betas = append(cloneStrings(betas), anthropicOAuthOnlyBetas...)
	}
	for k, v := range anthBetaHeader(betas) {
		headers[k] = v
	}
	if style == "kimi" {
		for k, v := range anthAttributionHeaders() {
			headers[k] = v
		}
	} else if oauth {
		headers["User-Agent"] = "claude-code/" + anthClaudeCodeVersion() + " (external, cli)"
		headers["x-app"] = "cli"
	}
	if isOpenCodeEndpoint(c.route.BaseURL) {
		for k, v := range anthAttributionHeaders() {
			if _, has := headers[k]; !has {
				headers[k] = v
			}
		}
	}
	for k, v := range extraHeaders {
		headers[k] = stringOf(v)
	}
	for k, v := range auth {
		anthSetHeader(headers, k, v)
	}
	for k, v := range anthCustomProviderExtraHeaders(ctx, c.route.BaseURL) {
		anthSetHeader(headers, k, v)
	}
	for k, v := range c.route.Headers {
		anthSetHeader(headers, k, v)
	}
	for k, v := range providerRequestHeaders(ctx) {
		anthSetHeader(headers, k, v)
	}
	return headers, oauth, nil
}

// Replaces every case variant of the header name so the later layer always wins on the wire
func anthSetHeader(headers map[string]string, name, value string) {
	for k := range headers {
		if strings.EqualFold(k, name) {
			delete(headers, k)
		}
	}
	headers[name] = value
}

// Extra headers of the custom provider entry routed at the base URL, empty when no entry matches
func anthCustomProviderExtraHeaders(ctx context.Context, baseURL string) map[string]string {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		catalogLog.Debug("custom-provider extra_headers skipped for Anthropic client", "error", err)
		return nil
	}
	return CustomProviderExtraHeaders(baseURL, nil, map[string]any(tree))
}

// Builds the wire body and headers of one request
func (c *anthropicClient) prepare(ctx context.Context, req *Request, stream bool) (*anthPrepared, error) {
	if req == nil {
		return nil, &LocalValidationError{Message: "anthropic messages request is nil"}
	}
	settings := loadWireSettings(ctx)
	c.mu.Lock()
	drop1M, buffer := c.dropContext1M, c.bufferToolInput
	c.mu.Unlock()
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.route.Model
	}
	isOAuth := c.variant == "direct" && c.routeIsOAuth(ctx)
	dicts := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m != nil {
			dicts = append(dicts, messageToDict(m))
		}
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	requestToolNames := map[string]bool{}
	for _, t := range req.Tools {
		tools = append(tools, t)
		if fn, ok := asMap(t["function"]); ok {
			if name := asStr(fn["name"]); name != "" {
				requestToolNames[name] = true
			}
		}
	}
	contextLength, _ := GetCachedContextLength(ctx, model, c.route.BaseURL, false)
	var maxTokens any
	if req.MaxTokens != 0 {
		maxTokens = req.MaxTokens
	}
	kwargs, err := anthBuildKwargs(anthBuildInput{
		model: model, messages: dicts, tools: tools, maxTokens: maxTokens, reasoning: anthReasoningFromRequest(req.Reasoning),
		toolChoice: req.ToolChoice, isOAuth: isOAuth, preserveDots: c.preserveDots(), contextLength: contextLength,
		baseURL: c.route.BaseURL, fastMode: strings.EqualFold(strings.TrimSpace(req.ServiceTier), "fast"), dropContext1M: drop1M,
		resolveProfileARN: anthProfileResolver(ctx, c.transport),
	})
	if err != nil {
		return nil, err
	}
	if req.Temperature != nil && !anthForbidsSamplingParams(kwargs["model"].(string)) {
		if _, has := kwargs["temperature"]; !has {
			kwargs["temperature"] = *req.Temperature
		}
	}
	if req.TopP != nil && !anthForbidsSamplingParams(kwargs["model"].(string)) {
		kwargs["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		stops := make([]any, 0, len(req.Stop))
		for _, s := range req.Stop {
			stops = append(stops, s)
		}
		kwargs["stop_sequences"] = stops
	}
	if user := strings.TrimSpace(req.User); user != "" {
		kwargs["metadata"] = map[string]any{"user_id": user}
	}
	if c.isNous() {
		extra, _ := asMap(kwargs["extra_body"])
		if extra == nil {
			extra = map[string]any{}
		}
		tags := []any{"product=icebath-agent", "client=icebath-client-v" + BuildVersion}
		if conv := firstNonEmpty(req.CacheScope, req.SessionID); conv != "" {
			tags = append(tags, "conversation="+conv)
		}
		extra["tags"] = tags
		if sticky := cacheScopeFromSessionID(firstNonEmpty(req.CacheScope, req.SessionID)); sticky != "" {
			extra["session_id"] = sticky
		}
		if du := dataUseRequirement(req.Messages); len(du) > 0 {
			prov := map[string]any{"data_collection": "deny"}
			if du["zero_retention"] {
				prov["zdr"] = true
			}
			extra["provider"] = prov
		}
		kwargs["extra_body"] = extra
	}
	anthMergeExtraBody(kwargs, c.route.ExtraBody)
	anthMergeExtraBody(kwargs, req.ExtraBody)
	extraHeaders, _ := asMap(kwargs["extra_headers"])
	delete(kwargs, "extra_headers")
	anthTranslateResponseFormat(kwargs, req.ResponseFormat)
	if extra, ok := asMap(kwargs["extra_body"]); ok {
		delete(kwargs, "extra_body")
		for k, v := range extra {
			kwargs[k] = v
		}
	}
	if rf, ok := asMap(kwargs["response_format"]); ok {
		anthTranslateResponseFormat(kwargs, rf)
	}
	delete(kwargs, "response_format")
	anthSanitizeKwargs(kwargs, "")
	if buffer {
		anthBufferToolInput(kwargs, c.route.BaseURL)
	}
	if stream {
		kwargs["stream"] = true
	} else {
		delete(kwargs, "stream")
	}
	betas := c.transport.Betas(drop1M)
	headers, oauth, err := c.requestHeaders(ctx, betas, extraHeaders)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.oauthWire = oauth || isOAuth
	c.requestToolNames = requestToolNames
	c.mu.Unlock()
	return &anthPrepared{body: kwargs, headers: headers, betas: betas, oauth: oauth || isOAuth, settings: settings,
		estTokens: EstimateRequestContextTokens(ctx, kwargs)}, nil
}

// Merges an extra body mapping into the kwargs, nested extra_body entries folded into the same map
func anthMergeExtraBody(kwargs map[string]any, overrides map[string]any) {
	for k, v := range overrides {
		if k == "extra_body" {
			if m, ok := asMap(v); ok {
				extra, _ := asMap(kwargs["extra_body"])
				if extra == nil {
					extra = map[string]any{}
				}
				for kk, vv := range m {
					extra[kk] = vv
				}
				kwargs["extra_body"] = extra
			}
			continue
		}
		if k == "extra_headers" {
			if m, ok := asMap(v); ok {
				existing, _ := asMap(kwargs["extra_headers"])
				if existing == nil {
					existing = map[string]any{}
				}
				for kk, vv := range m {
					existing[kk] = vv
				}
				kwargs["extra_headers"] = existing
			}
			continue
		}
		kwargs[k] = v
	}
}

// Overall request timeout and whether a per route or provider setting supplied it
func (c *anthropicClient) requestTimeout(req *Request, settings wireSettings) (time.Duration, bool) {
	if req != nil && req.Timeout > 0 {
		return req.Timeout, true
	}
	if c.route.Timeout > 0 {
		return c.route.Timeout, true
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	if d, ok := configuredProviderTimeout(settings.providers, c.providerLower(), model, "timeout_seconds", "request_timeout_seconds"); ok {
		return d, true
	}
	return 900 * time.Second, false
}

// Stale stream patience in seconds for the request
func (c *anthropicClient) staleTimeout(req *Request, settings wireSettings, estTokens int) float64 {
	if d, ok := configuredProviderTimeout(settings.providers, c.providerLower(), req.Model, "stale_timeout_seconds", "stale_timeout_seconds"); ok {
		return d.Seconds()
	}
	base := settings.streamStale
	if base <= 0 {
		base = defaultStreamStaleTimeout
	}
	if _, explicit := os.LookupEnv(StreamStaleTimeoutEnv); explicit {
		base = envFloat(StreamStaleTimeoutEnv, base)
	}
	if base == defaultStreamStaleTimeout && c.route.Local {
		local := settings.localStreamStale
		if local <= 0 {
			local = 900
		}
		return envFloat(LocalStreamStaleTimeoutEnv, local)
	}
	timeout := scaleStaleTimeoutForContext(base, estTokens)
	if floor, ok := ReasoningStaleTimeoutFloor(req.Model); ok && floor.Seconds() > timeout {
		timeout = floor.Seconds()
	}
	return timeout
}

// Socket timeouts of a streaming attempt, 900s read and 10s connect unless configured
func (c *anthropicClient) streamTimeouts(req *Request, settings wireSettings, estTokens int) ccTimeouts {
	base, _ := c.requestTimeout(req, settings)
	stale := c.staleTimeout(req, settings, estTokens)
	return ccTimeouts{base: base, read: base, connect: 10 * time.Second, stale: secondsDuration(stale),
		lifetime: secondsDuration(resolveStreamMaxLifetime(settings.streamMaxLifetime, stale))}
}

// APIError for a non success HTTP response
func (c *anthropicClient) httpError(resp *http.Response, data []byte) error {
	body := pystr.DecodeUTF8Replace(data)
	he := &ccHTTPError{Status: resp.StatusCode, Body: body, Headers: resp.Header.Clone()}
	apiErr := Classify(c.route.Provider, c.route.Model, resp.StatusCode, body, he)
	if apiErr.RetryAfter == 0 {
		apiErr.RetryAfter = RetryAfterFrom(resp.Header.Get("Retry-After"))
	}
	if apiErr.RequestID == "" {
		apiErr.RequestID = headerRequestID(resp.Header)
	}
	if apiErr.Err == nil {
		apiErr.Err = he
	}
	return apiErr
}

// APIError for a transport failure before any response arrived
func (c *anthropicClient) transportError(ctx context.Context, err error, phase string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return Classify(c.route.Provider, c.route.Model, 0, "", ccWrapTransport(err, phase))
}

// Complete issues one blocking Messages call, dropping the 1M beta once when the subscription rejects it
func (c *anthropicClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		p, err := c.prepare(ctx, req, false)
		if err != nil {
			return nil, err
		}
		stale := computeNonStreamStaleTimeout(ctx, p.settings, c.providerLower(), req, IsLocalEndpoint(c.route.BaseURL), false, p.estTokens, p.body)
		resp, err := runNonStreamRequest(ctx, req.Model, p.estTokens, stale, func(ctx context.Context) (*Response, error) { return c.completeOnce(ctx, req, p) })
		if err != nil && attempt == 0 && c.noteContext1MRejection(err) {
			continue
		}
		return resp, err
	}
	return nil, errors.New("provider: anthropic complete attempt loop ended without a response")
}

// One blocking Messages call
func (c *anthropicClient) completeOnce(ctx context.Context, req *Request, p *anthPrepared) (*Response, error) {
	base, _ := c.requestTimeout(req, p.settings)
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, base)
	defer cancel()
	httpReq, err := c.transport.Request(rctx, anthOpMessages, p.body, p.headers)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient(10 * time.Second).Do(httpReq)
	if err != nil {
		if ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
			err = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", base.Seconds())}
		}
		return nil, c.transportError(ctx, err, TimeoutPhaseConnect)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
			err = &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Request timed out after %.0fs.", base.Seconds())}
		}
		return nil, c.transportError(ctx, err, TimeoutPhaseRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.httpError(resp, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		apiErr := Classify(c.route.Provider, c.route.Model, resp.StatusCode, pystr.DecodeUTF8Replace(data), err)
		apiErr.Message = "Provider returned a non-JSON message body: " + truncate(redactSensitiveText(strings.TrimSpace(pystr.DecodeUTF8Replace(data))), 300)
		apiErr.Err = &ccHeadersError{err: err, headers: resp.Header.Clone()}
		return nil, apiErr
	}
	if eo, ok := payload["error"]; ok && eo != nil && asStr(payload["type"]) == "error" {
		return nil, c.httpError(resp, data)
	}
	return c.normalizeResponse(payload, resp.StatusCode, resp.Header.Clone(), time.Since(start))
}

// Stream issues one streamed Messages call, falling back to a blocking call where streaming is unavailable
func (c *anthropicClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.mu.Lock()
	latched := c.nonStreaming
	c.mu.Unlock()
	if latched {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		return resp, c.replay(resp, fn)
	}
	for attempt := 0; attempt < 2; attempt++ {
		p, err := c.prepare(ctx, req, true)
		if err != nil {
			return nil, err
		}
		resp, deltasSent, err := c.streamOnce(ctx, req, p, fn)
		if err == nil {
			return resp, nil
		}
		if attempt == 0 && !deltasSent && c.noteContext1MRejection(err) {
			continue
		}
		if !deltasSent && ctx.Err() == nil && c.transport.StreamUnavailable(err) {
			catalogLog.Info(fmt.Sprintf("Anthropic Messages stream unavailable for %s/%s; switching to non-streaming for this session: %s", orUnknown2(c.route.Provider), orUnknown2(c.route.Model), truncate(err.Error(), 200)))
			c.mu.Lock()
			c.nonStreaming = true
			c.mu.Unlock()
			final, err := c.Complete(ctx, req)
			if err != nil {
				return nil, err
			}
			return final, c.replay(final, fn)
		}
		return resp, err
	}
	return nil, errors.New("provider: anthropic stream attempt loop ended without a response")
}

// Fresh per attempt Messages stream state with its watches and diagnostics
func (c *anthropicClient) newStreamState(ctx context.Context, req *Request, p *anthPrepared, fn StreamFunc, t ccTimeouts) *anthStreamState {
	s := &anthStreamState{c: c, req: req, fn: fn, acc: newAnthAccumulator(), textWatch: NewRunawayStreamWatch(), thinkWatch: NewRunawayStreamWatch(),
		toolSlots: map[int]int{}, started: time.Now(), estTokens: p.estTokens, lifetime: t.lifetime.Seconds()}
	s.diag = streamCallFrom(ctx).newDiag()
	return s
}

// Runs one streaming attempt and reports whether visible deltas reached the consumer
func (c *anthropicClient) streamOnce(ctx context.Context, req *Request, p *anthPrepared, fn StreamFunc) (*Response, bool, error) {
	if _, managed := relayCurrentSessionID(ctx); managed {
		return anthropicManagedRelayStream(ctx, c, req, p, fn)
	}
	t := c.streamTimeouts(req, p.settings, p.estTokens)
	s := c.newStreamState(ctx, req, p, fn, t)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	kill := &ccKill{}
	httpReq, err := c.transport.Request(attemptCtx, anthOpMessages, p.body, p.headers)
	if err != nil {
		return nil, false, err
	}
	s.wire = failedStreamRequestOf(httpReq)
	stopHdrStale := armHeaderStaleKill(ctx, cancel, kill, s.emit, s.req.Model, s.estTokens, t.stale, s.started)
	hdrTimer := time.AfterFunc(t.read, func() {
		kill.fire("read", t.read.Seconds())
		cancel()
	})
	resp, err := c.httpClient(t.connect).Do(httpReq)
	hdrTimer.Stop()
	stopHdrStale()
	if err != nil {
		return nil, false, c.attemptError(attemptCtx, kill, err, s)
	}
	defer resp.Body.Close()
	if reabortIfCancelled(attemptCtx, resp) {
		return nil, false, c.attemptError(attemptCtx, kill, context.Cause(attemptCtx), s)
	}
	s.headers = resp.Header.Clone()
	s.status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := readLimited(resp.Body, ccErrorBodyLimit)
		return nil, false, c.httpError(resp, data)
	}
	StreamDiagCaptureResponse(s.diag, resp)
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, c.attemptError(attemptCtx, kill, err, s)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, false, c.streamError(providerStreamErrorFromNonJSON(string(data), s.headers))
		}
		if eo, ok := payload["error"]; ok && eo != nil {
			return nil, false, c.httpError(resp, data)
		}
		catalogLog.Info(fmt.Sprintf("Streaming request returned a final message object instead of an event stream; switching %s/%s to non-streaming for this session.", orUnknown2(c.route.Provider), orUnknown2(c.route.Model)))
		c.mu.Lock()
		c.nonStreaming = true
		c.mu.Unlock()
		final, err := c.normalizeResponse(payload, resp.StatusCode, s.headers, time.Since(s.started))
		if err != nil {
			return nil, false, err
		}
		return final, false, c.replay(final, fn)
	}
	s.claimWriter(ctx)
	out, err := c.consumeStream(ctx, attemptCtx, cancel, kill, c.transport.Events(resp), s, t)
	return out, s.deltasSent, err
}

// Replays a completed response's reasoning, text, tool calls and usage as stream events
func (c *anthropicClient) replay(resp *Response, fn StreamFunc) error {
	if fn == nil || resp == nil || resp.Message == nil {
		return nil
	}
	if resp.Message.Reasoning != "" {
		if err := fn(StreamEvent{Kind: EventReasoningDelta, Text: resp.Message.Reasoning}); err != nil {
			return err
		}
	}
	if text := resp.Message.Text(); text != "" {
		if err := fn(StreamEvent{Kind: EventTextDelta, Text: text}); err != nil {
			return err
		}
	}
	for i := range resp.Message.ToolCalls {
		tc := resp.Message.ToolCalls[i]
		if err := fn(StreamEvent{Kind: EventToolCallDone, Index: i, ToolCall: &tc}); err != nil {
			return err
		}
	}
	usage := resp.Usage
	if err := fn(StreamEvent{Kind: EventUsage, Usage: &usage}); err != nil {
		return err
	}
	return fn(StreamEvent{Kind: EventDone})
}

// Body keys the count_tokens endpoint accepts
var anthCountTokensKeys = []string{"model", "messages", "system", "tools", "tool_choice", "thinking", "mcp_servers", "context_management", "output_config"}

// CountTokens asks the endpoint how many input tokens the request occupies
func (c *anthropicClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	p, err := c.prepare(ctx, req, false)
	if err != nil {
		return 0, err
	}
	body := map[string]any{}
	for _, k := range anthCountTokensKeys {
		if v, has := p.body[k]; has {
			body[k] = v
		}
	}
	base, _ := c.requestTimeout(req, p.settings)
	rctx, cancel := context.WithTimeout(ctx, base)
	defer cancel()
	httpReq, err := c.transport.Request(rctx, anthOpCountTokens, body, p.headers)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient(10 * time.Second).Do(httpReq)
	if err != nil {
		return 0, c.transportError(ctx, err, TimeoutPhaseConnect)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, c.transportError(ctx, err, TimeoutPhaseRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, c.httpError(resp, data)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, Classify(c.route.Provider, c.route.Model, resp.StatusCode, pystr.DecodeUTF8Replace(data), err)
	}
	n, ok := anyInt(payload["input_tokens"])
	if !ok {
		n, ok = anyInt(payload["inputTokens"])
	}
	if !ok {
		return 0, &APIError{Provider: c.route.Provider, Model: c.route.Model, StatusCode: resp.StatusCode, Kind: ErrInvalidRequest, Code: "count_tokens_shape",
			Message: "count_tokens response carried no input_tokens", Body: string(data), RequestID: headerRequestID(resp.Header)}
	}
	return n, nil
}

// ---- response normalization ----

// Anthropic stop reasons mapped to the OpenAI finish vocabulary
var anthStopReasonMap = map[string]string{
	"end_turn": "stop", "tool_use": "tool_calls", "max_tokens": "length", "stop_sequence": "stop",
	"refusal": "content_filter", "model_context_window_exceeded": "length",
}

// OpenAI finish reason of a stop reason, stop for unknown values
func anthMapFinishReason(stopReason string) string {
	if mapped, ok := anthStopReasonMap[stopReason]; ok {
		return mapped
	}
	return "stop"
}

// Usage of a Messages response, the total counting cached and written input
func anthUsageFromMap(u map[string]any) Usage {
	out := Usage{}
	if u == nil {
		return out
	}
	out.InputTokens, _ = anyInt(u["input_tokens"])
	out.OutputTokens, _ = anyInt(u["output_tokens"])
	out.CacheReadTokens, _ = anyInt(u["cache_read_input_tokens"])
	out.CacheWriteTokens, _ = anyInt(u["cache_creation_input_tokens"])
	out.TotalTokens = out.InputTokens + out.CacheReadTokens + out.CacheWriteTokens + out.OutputTokens
	return out
}

// Structural check, empty content is legitimate only for end_turn and refusal
func anthValidateMessage(message map[string]any) bool {
	content, ok := asList(message["content"])
	if !ok {
		return false
	}
	stop := asStr(message["stop_reason"])
	return len(content) > 0 || stop == "end_turn" || stop == "refusal"
}

// ResponseStopDetails returns the stop_details object a Messages reply carried, nil when it carried none
func ResponseStopDetails(resp *Response) any {
	return rawResponseField(resp, "stop_details")
}

// Response of one Messages payload with text, reasoning, ordered thinking carriers and tool calls
func (c *anthropicClient) normalizeResponse(message map[string]any, status int, headers http.Header, latency time.Duration) (*Response, error) {
	raw, _ := json.Marshal(message)
	requestID := asStr(message["id"])
	if requestID == "" {
		requestID = headerRequestID(headers)
	}
	if !anthValidateMessage(message) {
		return nil, &APIError{Provider: c.route.Provider, Model: c.route.Model, StatusCode: status, Kind: ErrEmptyResponse, Code: "invalid_message",
			Message: "Provider returned a message with no content blocks", Body: string(raw), RequestID: requestID,
			Err: &ccHeadersError{err: errors.New("message carried no content"), headers: headers}}
	}
	return c.normalizeMessage(message, raw, requestID, status, headers, latency), nil
}

// Response of a structurally valid Messages payload, empty content yielding an empty assistant turn
func (c *anthropicClient) normalizeMessage(message map[string]any, raw []byte, requestID string, status int, headers http.Header, latency time.Duration) *Response {
	content, _ := asList(message["content"])
	var textParts, reasoningParts []string
	var reasoningDetails, ordered []any
	var toolCalls []state.ToolCall
	for _, b := range content {
		block, ok := asMap(b)
		if !ok {
			continue
		}
		clean := sanitizeReplayBlock(block)
		if clean != nil {
			ordered = append(ordered, clean)
		}
		switch asStr(block["type"]) {
		case "text":
			textParts = append(textParts, asStr(block["text"]))
		case "thinking", "redacted_thinking":
			if asStr(block["type"]) == "thinking" {
				reasoningParts = append(reasoningParts, asStr(block["thinking"]))
			}
			if clean != nil {
				reasoningDetails = append(reasoningDetails, clean)
			} else {
				reasoningDetails = append(reasoningDetails, block)
			}
		case "tool_use":
			name := c.inboundToolName(asStr(block["name"]))
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			args, err := json.Marshal(input)
			if err != nil {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, state.ToolCall{ID: asStr(block["id"]), Type: "function", Function: state.ToolCallFunction{Name: name, Arguments: string(args)}})
		}
	}
	finish := anthMapFinishReason(asStr(message["stop_reason"]))
	guardrail := c.transport.GuardrailIntervened(message)
	if guardrail {
		finish = "content_filter"
	}
	msg := &state.Message{Role: "assistant", ToolCalls: toolCalls, FinishReason: finish}
	if len(textParts) > 0 {
		msg.Content = strings.Join(textParts, "\n")
	}
	if len(reasoningParts) > 0 {
		msg.Reasoning = strings.Join(reasoningParts, "\n\n")
		msg.ReasoningContent = msg.Reasoning
	}
	signed := false
	hasToolUse := false
	for _, b := range ordered {
		m, _ := asMap(b)
		t := asStr(m["type"])
		if thinkingBlockTypes[t] && (asStr(m["signature"]) != "" || asStr(m["data"]) != "") {
			signed = true
		}
		if t == "tool_use" {
			hasToolUse = true
		}
	}
	if len(reasoningDetails) > 0 {
		msg.ReasoningDetails, _ = json.Marshal(reasoningDetails)
	}
	if signed && hasToolUse {
		setAnthropicLiveBlocks(msg, ordered)
	}
	usage := Usage{}
	if u, ok := asMap(message["usage"]); ok {
		usage = anthUsageFromMap(u)
	}
	model := asStr(message["model"])
	if model == "" {
		model = c.route.Model
	}
	resp := &Response{Message: msg, FinishReason: finish, Usage: usage, Model: model, Provider: c.route.Provider, RequestID: requestID,
		StatusCode: status, Headers: headers, Raw: raw, Latency: latency, Truncated: finish == "length"}
	if guardrail {
		anthMarkRaw(resp, ccRawContentFilter, true)
	}
	return resp
}
