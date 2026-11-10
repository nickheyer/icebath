package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs one teardown step and swallows its error or panic so sibling steps still run
func quietly(fn func() error) {
	defer func() { _ = recover() }()
	_ = fn()
}

// True when an automatic background review targets the managed local runtime under defer auto
func reviewShouldDefer(ctx context.Context, a *AIAgent, taskCfg map[string]any) bool {
	return reviewDeferMode(taskCfg) == "auto" && reviewTargetsManagedLocal(ctx, a, taskCfg)
}

// StreamErrorEvent is a provider error synthesized from a standalone Responses type error SSE frame
type StreamErrorEvent struct {
	Message    string
	Code       *string
	Param      *string
	StatusCode *int
	Body       map[string]any
}

// Builds the event with the OpenAI SDK shaped body the error classifiers read
func NewStreamErrorEvent(message string, code, param *string, statusCode *int) *StreamErrorEvent {
	return &StreamErrorEvent{Message: message, Code: code, Param: param, StatusCode: statusCode, Body: map[string]any{
		"error": map[string]any{"message": message, "code": optionalString(code), "param": optionalString(param), "type": "error"},
	}}
}

func (e *StreamErrorEvent) Error() string { return e.Message }

// Nil string pointer as None and a set pointer as its value
func optionalString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// SessionDBForRecall is the executor host view of the recall database
func (a *AIAgent) SessionDBForRecall(ctx context.Context) *state.DB {
	return a.getSessionDBForRecall(ctx)
}

// Session DB for recall, acquiring the shared default state DB when none was passed and persistence is enabled
func (a *AIAgent) getSessionDBForRecall(ctx context.Context) *state.DB {
	if a.persistDisabled {
		return nil
	}
	if a.sessionDB != nil {
		return a.sessionDB
	}
	db, err := state.Acquire(ctx, "")
	if err != nil {
		runAgentLog().Debug("SessionDB unavailable for recall", "err", err)
		return nil
	}
	a.sessionDB = db
	a.ownsSessionDB = true
	return a.sessionDB
}

// Safe LM Studio context budget from explicit intent and the verified runtime load, zero for None
func effectiveLMStudioContextLength(configContextLength any, runtime any) int {
	explicit := positiveInt(configContextLength)
	var rt int
	switch r := runtime.(type) {
	case provider.LMStudioLoadResult:
		if r.Rejected || (r.LoadAttempted && positiveInt(r.ContextLength) == 0) {
			return 0
		}
		rt = positiveInt(r.ContextLength)
	case *provider.LMStudioLoadResult:
		if r == nil {
			break
		}
		return effectiveLMStudioContextLength(configContextLength, *r)
	default:
		rt = positiveInt(runtime)
	}
	if rt != 0 && explicit != 0 {
		return min(rt, explicit)
	}
	if rt != 0 {
		return rt
	}
	return explicit
}

// True when a management load was rejected or could not be verified
func lmstudioLoadWasUnverified(loadResult *provider.LMStudioLoadResult) bool {
	if loadResult == nil {
		return false
	}
	return loadResult.Rejected || (loadResult.LoadAttempted && loadResult.ContextLength == 0)
}

// Preloads the LM Studio model unless the provider differs or the load mode is jit, nil when skipped
func (a *AIAgent) ensureLMStudioRuntimeLoaded(ctx context.Context, configContextLength *int) *provider.LMStudioLoadResult {
	if strings.ToLower(strings.TrimSpace(a.provider)) != "lmstudio" {
		return nil
	}
	mode := a.lmstudioLoadMode
	if mode == "" {
		mode = "explicit"
	}
	if strings.ToLower(strings.TrimSpace(mode)) == "jit" {
		runAgentLog().Debug("LM Studio explicit preload skipped: lmstudio_load_mode=jit")
		return nil
	}
	target := a.configContextLength
	if configContextLength != nil {
		target = *configContextLength
	}
	res := provider.EnsureLMStudioModelLoaded(ctx, a.model, a.baseURL, a.apiKey, target, target > 0, 0)
	return &res
}

// Pops codex_reasoning_items from every assistant message and disables replay unless kept, returning the counts
func (a *AIAgent) disableCodexReasoningReplay(messages []map[string]any, keepReplay bool) map[string]int {
	strippedMessages, strippedItems := 0, 0
	for _, msg := range messages {
		if msg == nil || msg["role"] != "assistant" {
			continue
		}
		items, had := msg["codex_reasoning_items"]
		if !had {
			continue
		}
		delete(msg, "codex_reasoning_items")
		if list, ok := items.([]any); ok && len(list) > 0 {
			strippedMessages++
			strippedItems += len(list)
		} else if list, ok := items.([]map[string]any); ok && len(list) > 0 {
			strippedMessages++
			strippedItems += len(list)
		}
	}
	if !keepReplay {
		a.codexReasoningReplayEnabled = false
	}
	return map[string]int{"messages": strippedMessages, "items": strippedItems}
}

// True for a malformed Anthropic event stream frame on the anthropic_messages wire
func (a *AIAgent) isProviderStreamParseError(err error) bool {
	return a.apiMode == "anthropic_messages" && provider.IsProviderStreamParseError(err)
}

// Surfaces a compact warning naming the failed auxiliary task and the summarized error
func (a *AIAgent) emitAuxiliaryFailure(task string, err error) {
	detail := func() (d string) {
		defer func() {
			if recover() != nil {
				d = err.Error()
			}
		}()
		return a.summarizeAPIError(err)
	}()
	if detail == "" {
		detail = fmt.Sprintf("%T", err)
		if i := strings.LastIndex(detail, "."); i >= 0 {
			detail = detail[i+1:]
		}
	}
	detail = strings.TrimSpace(detail)
	if utf8.RuneCountInString(detail) > 220 {
		detail = strings.TrimRight(string([]rune(detail)[:217]), " \t\n\r\v\f") + "..."
	}
	a.emitWarning("⚠ Auxiliary " + task + " failed: " + detail)
}

// Live main runtime fields for session scoped auxiliary routing
func (a *AIAgent) currentMainRuntime() map[string]string {
	return map[string]string{
		"model": a.model, "provider": a.provider, "base_url": a.baseURL, "api_key": a.apiKey,
		"api_mode": a.apiMode, "auth_mode": "", "session_id": a.sessionID,
	}
}

// Hostname of the given base URL, or of the agent base URL when nil, panicking on the urlparse ValueError
func (a *AIAgent) hostnameFor(baseURL *string) string {
	if baseURL != nil {
		h, err := pyurl.BaseURLHostname(*baseURL)
		if err != nil {
			panic(err)
		}
		return h
	}
	if a.baseURLHostname != "" {
		return a.baseURLHostname
	}
	h, err := pyurl.BaseURLHostname(a.baseURLLower)
	if err != nil {
		panic(err)
	}
	return h
}

// True when the base URL targets the native OpenAI API
func (a *AIAgent) isDirectOpenAIURL(baseURL *string) bool {
	return a.hostnameFor(baseURL) == "api.openai.com"
}

// True when the base URL targets Azure OpenAI
func (a *AIAgent) isAzureOpenAIURL(baseURL *string) bool {
	url := a.baseURLLower
	if baseURL != nil {
		url = strings.ToLower(*baseURL)
	}
	ok, err := pyurl.BaseURLHostMatches(url, "openai.azure.com")
	if err != nil {
		panic(err)
	}
	return ok
}

// Reads an environment variable as a float, the default when unset, blank or unparsable
func envFloatOr(key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := parsePyFloat(raw)
	if err != nil {
		return def
	}
	return v
}

// Python float() over a string, accepting inf, nan and underscores between digits
func parsePyFloat(raw string) (float64, error) {
	s := strings.TrimSpace(raw)
	switch strings.ToLower(strings.TrimLeft(s, "+-")) {
	case "inf", "infinity", "nan":
		return strconv.ParseFloat(s, 64)
	}
	if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
		return 0, fmt.Errorf("could not convert string to float: %q", raw)
	}
	return strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
}

// Per call request timeout from model config, provider config, ICEBATH_API_TIMEOUT, then 1800 seconds
func (a *AIAgent) resolvedAPICallTimeout(ctx context.Context) float64 {
	if v, ok := provider.GetProviderRequestTimeout(ctx, a.provider, a.model); ok {
		return v
	}
	return envFloatOr("ICEBATH_API_TIMEOUT", 1800.0)
}

// Base non stream stale timeout and whether it is the implicit 90 second default
func (a *AIAgent) resolvedAPICallStaleTimeoutBase(ctx context.Context) (float64, bool) {
	if v, ok := provider.GetProviderStaleTimeout(ctx, a.provider, a.model); ok {
		return v, false
	}
	if env, ok := os.LookupEnv("ICEBATH_API_CALL_STALE_TIMEOUT"); ok {
		v, err := parsePyFloat(env)
		if err != nil {
			panic(fmt.Errorf("could not convert string to float: '%s'", env))
		}
		return v, false
	}
	if floor, ok := provider.ReasoningStaleTimeoutFloor(a.model); ok {
		return floor.Seconds(), false
	}
	return 90.0, true
}

// Effective non stream stale timeout scaled by request size, long decode hops and capped by the run budget
func (a *AIAgent) computeNonStreamStaleTimeout(ctx context.Context, apiPayload any) float64 {
	staleBase, implicit := a.resolvedAPICallStaleTimeoutBase(ctx)
	if implicit && a.baseURL != "" && provider.IsLocalEndpoint(a.baseURL) {
		return math.Inf(1)
	}
	est := provider.EstimateRequestContextTokens(ctx, apiPayload)
	timeout := staleBase
	if est > 100_000 {
		timeout = math.Max(staleBase, 240.0)
	} else if est > 50_000 {
		timeout = math.Max(staleBase, 150.0)
	}
	explicit := a.staleTimeoutIsExplicit(ctx)
	if a.apiMode == "codex_responses" && !explicit {
		timeout = math.Max(timeout, highEffortSilenceFloor(a))
	}
	if !explicit {
		payload, _ := apiPayload.(map[string]any)
		timeout *= float64(provider.LongDecodeStaleMultiplier(payload))
		timeout = capToRunBudget(a, timeout)
	}
	return timeout
}

// True when the stale timeout comes from config or the environment rather than an implicit floor
func (a *AIAgent) staleTimeoutIsExplicit(ctx context.Context) bool {
	if _, ok := provider.GetProviderStaleTimeout(ctx, a.provider, a.model); ok {
		return true
	}
	_, ok := os.LookupEnv("ICEBATH_API_CALL_STALE_TIMEOUT")
	return ok
}

var codexSilentHangModelRE = regexp.MustCompile(`(?:^|[/\-_])gpt-5\.5(?:$|[\-_])`)

// Actionable hint for the known Codex silent reject model family, empty when the request does not match
func (a *AIAgent) codexSilentHangHint(model *string) string {
	if a.apiMode != "codex_responses" {
		return ""
	}
	if !provider.ResponsesRouteIsCodexBackend(a.provider, a.baseURL) {
		return ""
	}
	eff := a.model
	if model != nil {
		eff = *model
	}
	if !codexSilentHangModelRE.MatchString(strings.ToLower(eff)) {
		return ""
	}
	return "Codex backend appears to be silently rejecting " + pyStrRepr(eff) + " " +
		"on chatgpt.com/backend-api/codex (no stream events, no error). " +
		"This is a known backend-side pattern that has affected ChatGPT " +
		"Plus accounts intermittently. " +
		"Workaround: try `gpt-5.4` on the same OAuth profile, " +
		"or switch to a different model/provider in your fallback chain. " +
		"Some ChatGPT Codex accounts do not support `gpt-5.4-codex`. " +
		"See icebath-agent#21444 for symptom history."
}

// Python repr of a str, single quoted unless the text holds a single quote and no double quote
func pyStrRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// True when the base URL targets OpenRouter
func (a *AIAgent) isOpenRouterURL() bool {
	ok, err := pyurl.BaseURLHostMatches(a.baseURLLower, "openrouter.ai")
	if err != nil {
		panic(err)
	}
	return ok
}

// True for the ChatGPT OAuth Codex Responses backend
func (a *AIAgent) isCodexBackend() bool {
	return a.apiMode == "codex_responses" && a.baseURLHostname == "chatgpt.com" &&
		strings.Contains(a.baseURLLower, "/backend-api/codex")
}

func (a *AIAgent) IsCodexBackend() bool { return a.isCodexBackend() }

// True for GPT 5 family models that chat completions rejects, vendor prefix stripped
func modelRequiresResponsesAPI(model string) bool {
	lower := strings.ToLower(model)
	if i := strings.LastIndex(lower, "/"); i >= 0 {
		lower = lower[i+1:]
	}
	return strings.HasPrefix(lower, "gpt-5")
}

// True when the provider and model pair should use the Responses API
func providerModelRequiresResponsesAPI(ctx context.Context, model, providerName string) bool {
	normalized := strings.ToLower(strings.TrimSpace(providerName))
	if normalized == "nous" || normalized == "custom" || provider.IsActualRoute(providerName, "") {
		return false
	}
	if provider.IsExternalProcessProvider(ctx, normalized) {
		return false
	}
	if normalized != "" {
		if profile, ok := provider.GetProviderProfile(ctx, normalized); ok {
			if derived, ok := provider.ProfileModelAPIMode(ctx, profile, model); ok {
				return derived == "codex_responses"
			}
		}
	}
	return modelRequiresResponsesAPI(model)
}

// Output token cap keyword the endpoint expects, max_completion_tokens for newer OpenAI families
func (a *AIAgent) maxTokensParam(ctx context.Context, value int) map[string]any {
	if a.isDirectOpenAIURL(nil) || a.isAzureOpenAIURL(nil) {
		return map[string]any{"max_completion_tokens": value}
	}
	declared := provider.RouteAnswer(ctx, "max_tokens_param", a.provider, a, map[string]any{"base_url": a.baseURL, "model": a.model})
	if s, ok := declared.(string); ok && s != "" {
		return map[string]any{s: value}
	}
	if provider.ModelForcesMaxCompletionTokens(a.model) {
		return map[string]any{"max_completion_tokens": value}
	}
	return map[string]any{"max_tokens": value}
}

// Outgoing response token cap of a prepared request, zero for None
func (a *AIAgent) requestedOutputCapFromAPIKwargs(apiKwargs map[string]any) int {
	if apiKwargs == nil {
		return 0
	}
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if v, ok := pyIntCoerce(apiKwargs[key]); ok && v > 0 {
			return v
		}
	}
	return 0
}

// Python int() over an int, a bool, a float or a decimal string, false where int() raises
func pyIntCoerce(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(x), true
	case string:
		s := strings.TrimSpace(x)
		if strings.Contains(s, "__") || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") {
			return 0, false
		}
		n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
		return n, err == nil
	}
	return 0, false
}

// True when text remains once reasoning blocks are stripped
func (a *AIAgent) hasContentAfterThinkBlock(content string) bool {
	return content != "" && strings.TrimSpace(a.stripThinkBlocks(content)) != ""
}

// HasContentAfterThinkBlock reports visible text left once reasoning blocks are stripped
func (a *AIAgent) HasContentAfterThinkBlock(content string) bool {
	return a.hasContentAfterThinkBlock(content)
}

// Heuristic for visible assistant text that looks intentionally finished
func hasNaturalResponseEnding(content string) bool {
	stripped := strings.TrimRightFunc(content, pyIsSpace)
	if stripped == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(stripped)
	return strings.HasSuffix(stripped, "```") || strings.ContainsRune(".!?:)\"']}。！？：）】」』》^", last) || last >= 0x1F300
}

// Python str.isspace for one rune
func pyIsSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// True for Ollama hosted GLM models that misreport finish_reason stop, Ollama Cloud excluded
func (a *AIAgent) isOllamaGLMBackend() bool {
	modelLower := strings.ToLower(a.model)
	providerLower := strings.ToLower(a.provider)
	if !strings.Contains(modelLower, "glm") && providerLower != "zai" {
		return false
	}
	base := a.baseURLLower
	if strings.Contains(base, "ollama.com") || strings.Contains(modelLower, ":cloud") {
		return false
	}
	if strings.Contains(base, "ollama") || strings.Contains(base, ":11434") {
		return true
	}
	return providerLower == "ollama"
}

// Conservative stop to length rewrite for Ollama hosted GLM models after a tool round
func (a *AIAgent) shouldTreatStopAsTruncated(finishReason string, assistantMessage *state.Message, messages []map[string]any) bool {
	if finishReason != "stop" || a.apiMode != "chat_completions" || !a.isOllamaGLMBackend() {
		return false
	}
	hasTool := false
	for _, m := range messages {
		if m != nil && m["role"] == "tool" {
			hasTool = true
			break
		}
	}
	if !hasTool || assistantMessage == nil || len(assistantMessage.ToolCalls) > 0 {
		return false
	}
	content, ok := assistantMessage.Content.(string)
	if !ok {
		return false
	}
	visible := strings.TrimFunc(a.stripThinkBlocks(content), pyIsSpace)
	if utf8.RuneCountInString(visible) < 20 || strings.IndexFunc(visible, pyIsSpace) < 0 {
		return false
	}
	return !hasNaturalResponseEnding(visible)
}
