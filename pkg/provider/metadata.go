package provider

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/home"
)

// Descending probe tiers for unknown models, the first is also the default fallback
var ContextProbeTiers = []int{256_000, 128_000, 64_000, 32_000, 16_000, 8_000}

// DefaultFallbackContext is the window assumed when every detection source misses
const DefaultFallbackContext = 256_000

const defaultFallbackContext = DefaultFallbackContext

// MinimumContextLength is the smallest window sessions, model switches and cron jobs accept
const MinimumContextLength = 64_000

var ollamaTagPattern = regexp.MustCompile(`(?i)^(\d+\.?\d*b|latest|stable|q\d|fp?\d|instruct|chat|coder|vision|text)`)

var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

var (
	fallbackWarnedMu sync.Mutex
	fallbackWarned   = map[[2]string]bool{}
)

// Strips a registry known provider prefix while preserving Ollama model tag ids
func stripProviderPrefix(model string) string {
	if !strings.Contains(model, ":") || strings.HasPrefix(model, "http") {
		return model
	}
	prefix, suffix, _ := strings.Cut(model, ":")
	_, isProvider := ProfileFor(strings.ToLower(strings.TrimSpace(prefix)))
	if isProvider && !ollamaTagPattern.MatchString(strings.TrimSpace(suffix)) {
		return suffix
	}
	return model
}

// Parsed normalized url with the scheme prepended when absent, nil when empty or malformed
func parseProbeURL(baseURL, scheme string) *url.URL {
	normalized := normalizeBaseURL(baseURL)
	if normalized == "" {
		return nil
	}
	if !strings.Contains(normalized, "://") {
		normalized = scheme + "://" + normalized
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil
	}
	return parsed
}

// Warns once per model and endpoint that detection failed and the hard default is used
func warnContextLengthFallback(model, baseURL string) {
	key := [2]string{model, baseURL}
	fallbackWarnedMu.Lock()
	seen := fallbackWarned[key]
	fallbackWarned[key] = true
	fallbackWarnedMu.Unlock()
	if seen {
		return
	}
	shown := baseURL
	if shown == "" {
		shown = "default"
	}
	metadataLog.Warn(fmt.Sprintf("Could not determine context length for model %q (base_url=%s) — falling back to %s tokens. Set model.context_length in config.yaml to override.", model, shown, formatWithCommas(DefaultFallbackContext)))
}

// Integer with comma thousands separators
func formatWithCommas(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(s[i])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

type contextEntry struct {
	key string
	ctx int
}

// Family pattern fallbacks used only when provider aware sources all miss, longest key wins
var defaultContextLengths = []contextEntry{
	{"claude-fable-5", 1000000}, {"claude-fable", 1000000}, {"claude-opus-5", 1000000}, {"claude-sonnet-5", 1000000},
	{"claude-opus-4-8", 1000000}, {"claude-opus-4.8", 1000000}, {"claude-opus-4-7", 1000000}, {"claude-opus-4.7", 1000000},
	{"claude-opus-4-6", 1000000}, {"claude-sonnet-4-6", 1000000}, {"claude-opus-4.6", 1000000}, {"claude-sonnet-4.6", 1000000},
	{"claude", 200000},
	{"gpt-6-astra", 1050000},
	{"gpt-6.1-sol", 1050000}, {"gpt-6.1-terra", 1050000},
	{"gpt-6-sol", 1050000}, {"gpt-6-luna", 1050000},
	{"gpt-5.6-luna", 1050000}, {"gpt-5.6-terra", 1050000}, {"gpt-5.6-sol", 1050000}, {"gpt-5.5", 1050000},
	{"gpt-5.4-nano", 400000}, {"gpt-5.4-mini", 400000}, {"gpt-5.4", 1050000},
	{"gpt-5.3-codex-spark", 128000}, {"gpt-5.1-chat", 128000}, {"gpt-5", 400000},
	{"gpt-4.1", 1047576}, {"gpt-4", 128000},
	{"gemini", 1048576},
	{"gemma-4", 256000}, {"gemma4", 256000}, {"gemma-4-31b", 256000}, {"gemma-3", 131072}, {"gemma", 8192},
	{"deepseek-v4-pro", 1_000_000}, {"deepseek-v4.1-flash", 1_000_000}, {"deepseek-v4-flash", 1_000_000}, {"deepseek-chat", 1_000_000},
	{"deepseek-reasoner", 1_000_000}, {"deepseek-flash", 1_000_000}, {"deepseek", 128000},
	{"llama", 131072}, {"muse-spark-1.3", 1_048_576}, {"muse-spark", 1_048_576}, {"inkling", 1_048_576},
	{"qwen3.8-max", 1_000_000}, {"qwen3.8-flash", 1_000_000}, {"qwen3.6-plus", 1048576}, {"qwen3.7-plus", 1048576},
	{"qwen3-coder-plus", 1000000}, {"qwen3-coder", 262144}, {"qwen3-max", 262144}, {"qwen", 131072},
	{"minimax-m3", 1000000}, {"minimax", 204800},
	{"glm-5.3", 1_310_720}, {"glm-5.3-flash", 1_310_720}, {"glm-5.3:batch", 1_048_576}, {"glm-5.3:us", 1_048_576},
	{"glm-5.3-flash:batch", 1_048_576}, {"glm-5.3-flash:us", 1_048_576}, {"glm-5.3-flashx", 1_048_576},
	{"glm-5.2", 1_048_576}, {"glm-5.2:free", 256_000},
	{"glm-5.1", 204_800}, {"glm-5-turbo", 202752}, {"glm-5v-turbo", 202752}, {"glm-5", 204_800},
	{"glm-4.7-flash", 202752}, {"glm-4.7", 204_800}, {"glm-4.6v", 131072}, {"glm-4.6", 204_800}, {"glm", 202752},
	{"grok-composer", 200000}, {"grok-build-latest", 500000}, {"grok-build", 256000}, {"grok-code-fast", 256000},
	{"grok-2-vision", 8192}, {"grok-4-fast", 2000000}, {"grok-4.20", 2000000},
	{"grok-4.6", 500000}, {"grok-4.5", 500000}, {"grok-4.3", 1000000}, {"grok-4", 256000},
	{"grok-3", 131072}, {"grok-2", 131072}, {"grok", 131072},
	{"kimi-k3", 1_048_576}, {"kimi", 262144},
	{"solar-open2", 262144}, {"solar-pro3", 131072}, {"solar-pro2", 65536}, {"solar-mini", 32768}, {"solar-", 524288},
	{"hy4-preview", 1_048_576}, {"hy3-preview", 262144}, {"hy3", 262144},
	{"x-preview-f", 1_048_576}, {"ox-alpha", 1_048_576}, {"union-alpha", 262144},
	{"nemotron-3.5-lightning", 1_000_000}, {"nemotron-3.5-super-vl", 1_000_000}, {"nemotron", 131072},
	{"laguna-s-2.1", 262144}, {"laguna-xs-2.1", 262144}, {"trinity", 262144}, {"elephant", 262144},
	{"Qwen/Qwen3.5-397B-A17B", 131072}, {"Qwen/Qwen3.5-35B-A3B", 131072}, {"deepseek-ai/DeepSeek-V3.2", 65536},
	{"moonshotai/Kimi-K2.5", 262144}, {"moonshotai/Kimi-K2.6", 262144}, {"moonshotai/Kimi-K2-Thinking", 262144},
	{"MiniMaxAI/MiniMax-M2.5", 204800}, {"XiaomiMiMo/MiMo-V2-Flash", 262144},
	{"mimo-v2.6-pro-ultraspeed", 1048576}, {"mimo-v2.6-pro", 1048576}, {"mimo-v2.6-flash", 1048576},
	{"mimo-v2-pro", 1048576}, {"mimo-v2.5-pro", 1048576}, {"mimo-v2.5", 1048576}, {"mimo-v2-omni", 262144}, {"mimo-v2-flash", 262144},
	{"zai-org/GLM-5", 202752},
}

// DefaultContextLengths exposes the family fallback table as a map
func DefaultContextLengths() map[string]int {
	out := make(map[string]int, len(defaultContextLengths))
	for _, e := range defaultContextLengths {
		out[e.key] = e.ctx
	}
	return out
}

// xAI Grok models that accept reasoning effort
var grokEffortCapablePrefixes = []string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"}

// GrokSupportsReasoningEffort allowlists Grok ids that accept the effort dial
func GrokSupportsReasoningEffort(model string) bool {
	name := lastPathSegment(strings.ToLower(strings.TrimSpace(model)))
	if name == "" {
		return false
	}
	for _, p := range grokEffortCapablePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

var openAINonReasoningRe = regexp2.MustCompile(`^(?:ft:)?(?:gpt-3\.5|gpt-4(?![0-9])|chatgpt-)`, regexp2.None)

// OpenAIModelRejectsReasoning is true for chat era ids that 400 on any reasoning field
func OpenAIModelRejectsReasoning(model string) bool {
	name := lastPathSegment(strings.ToLower(strings.TrimSpace(model)))
	m, _ := openAINonReasoningRe.MatchString(name)
	return m
}

// IsGrok46Family reports whether the id belongs to the Grok 4.6 family
func IsGrok46Family(model string) bool {
	name := lastPathSegment(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-"))
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

// Claude ids that accept the fast speed parameter
var fastModeAnthropicModels = map[string]bool{"claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true}

var dottedVersionRe = regexp.MustCompile(`(\d)\.(\d)`)

var dateSuffixRe = regexp.MustCompile(`-\d{8}$`)

// IsAnthropicFastModeModel accepts vendor prefixed, dotted, variant and dated spellings
func IsAnthropicFastModeModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	name = strings.SplitN(name, ":", 2)[0]
	name = lastPathSegment(name)
	name = dottedVersionRe.ReplaceAllString(name, "$1-$2")
	return fastModeAnthropicModels[dateSuffixRe.ReplaceAllString(name, "")]
}

var contextLengthKeys = []string{
	"context_length", "context_window", "context_size", "max_context_length", "max_position_embeddings",
	"max_model_len", "max_input_tokens", "max_sequence_length", "max_seq_len", "n_ctx_train", "n_ctx", "ctx_size",
}

var maxCompletionKeys = []string{"max_completion_tokens", "max_output_tokens", "max_tokens"}

var metadataLocalHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "0.0.0.0": true}

var metadataContainerLocalSuffixes = []string{".docker.internal", ".containers.internal", ".lima.internal"}

// Bearer header for a probe, empty without a usable key
func authHeaders(apiKey string) map[string]string {
	token := strings.TrimSpace(apiKey)
	if token == "" {
		return map[string]string{}
	}
	return map[string]string{"Authorization": "Bearer " + token}
}

func isCustomEndpoint(baseURL string) bool {
	normalized := normalizeBaseURL(baseURL)
	return normalized != "" && !HostMatches(normalized, "openrouter.ai")
}

type hostProvider struct{ part, provider string }

// Host substring to provider, profiles claim further hosts through their hostnames
var metadataURLToProvider = []hostProvider{
	{"api.openai.com", "openai"}, {"chatgpt.com", "openai"}, {"api.anthropic.com", "anthropic"},
	{"api.z.ai", "zai"}, {"open.bigmodel.cn", "zai"},
	{"api.moonshot.ai", "kimi-coding"}, {"api.moonshot.cn", "kimi-coding-cn"}, {"api.kimi.com", "kimi-coding"},
	{"api.stepfun.ai", "stepfun"}, {"api.stepfun.com", "stepfun"}, {"api.arcee.ai", "arcee"}, {"api.minimax", "minimax"},
	{"dashscope.aliyuncs.com", "alibaba"}, {"dashscope-intl.aliyuncs.com", "alibaba"}, {"portal.qwen.ai", "qwen-oauth"},
	{"openrouter.ai", "openrouter"}, {"generativelanguage.googleapis.com", "gemini"},
	{"inference-api.nousresearch.com", "nous"}, {"api.deepseek.com", "deepseek"},
	{"api.fireworks.ai", "fireworks"}, {"opencode.ai", "opencode-go"}, {"api.x.ai", "xai"},
	{"integrate.api.nvidia.com", "nvidia"}, {"api.xiaomimimo.com", "xiaomi"}, {"xiaomimimo.com", "xiaomi"},
	{"api.gmi-serving.com", "gmi"}, {"api.novita.ai", "novita"},
	{"tokenhub.tencentmaas.com", "tencent-tokenhub"}, {"api.lkeap.cloud.tencent.com", "tencent-tokenplan"},
	{"ollama.com", "ollama-cloud"},
}

var (
	urlToProviderOnce  sync.Once
	urlToProviderTable []hostProvider
)

// Static host table extended with every profile hostname not already mapped
func metadataHostTable() []hostProvider {
	urlToProviderOnce.Do(func() {
		table := append([]hostProvider{}, metadataURLToProvider...)
		known := map[string]bool{}
		for _, e := range table {
			known[e.part] = true
		}
		for _, p := range Profiles() {
			host := Hostname(p.BaseURL)
			if host != "" && !known[host] && !IsLocalEndpoint("http://"+host) {
				known[host] = true
				table = append(table, hostProvider{host, p.Name})
			}
		}
		urlToProviderTable = table
	})
	return urlToProviderTable
}

// Registered profile whose declared hostnames match the host exactly or as a subdomain
func profileForHost(rawURL string) (Profile, bool) {
	return ProfileForHost(context.Background(), rawURL, false)
}

// Provider name for a base url, profile host claims beat substring rows
func inferProviderFromBaseURL(baseURL string) string {
	parsed := parseProbeURL(baseURL, "https")
	if parsed == nil {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	if host == "" {
		host = strings.ToLower(parsed.Path)
	}
	if p, ok := ProfileForHost(context.Background(), "https://"+host, true); ok {
		return p.Name
	}
	for _, e := range metadataHostTable() {
		if strings.Contains(host, e.part) {
			return e.provider
		}
	}
	return ""
}

// OpenRouterVariantBase returns the base id of a routing variant suffix or empty
func OpenRouterVariantBase(model string) string { return home.OpenRouterVariantBase(model) }

// Strips an OpenRouter routing variant for catalog lookup only when the request routes through OpenRouter
func stripOpenRouterRoutingVariant(model, baseURL, providerName string) string {
	if model == "" {
		return model
	}
	isOpenRouter := strings.ToLower(strings.TrimSpace(providerName)) == "openrouter" || (baseURL != "" && inferProviderFromBaseURL(baseURL) == "openrouter")
	if !isOpenRouter {
		return model
	}
	base := OpenRouterVariantBase(model)
	if base == "" {
		return model
	}
	metadataLog.Debug("resolving context length for OpenRouter routing variant via base id", "model", model, "base", base)
	return base
}

func isKnownProviderBaseURL(baseURL string) bool {
	return inferProviderFromBaseURL(baseURL) != ""
}

// LM Studio server root for native api v1 endpoints
func lmstudioServerRoot(baseURL string) string {
	root := normalizeBaseURL(baseURL)
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimRight(root[:len(root)-len(suffix)], "/")
		}
	}
	return root
}

// Probe root for a local server, IPv4 resolved with the v1 suffix stripped
func probeServerRoot(baseURL string) string {
	return strings.TrimSuffix(localhostToIPv4(strings.TrimRight(baseURL, "/")), "/v1")
}

// Families whose generation digit is part of the name match only on an id boundary
var boundaryMatchedKeyPrefixes = []string{"solar-"}

var hyphenatedGenerationRe = regexp2.MustCompile(`((?:solar\-)[a-z]+)-(\d{1,2})(?=[-:.@]|$)`, regexp2.None)

func hasBoundaryPrefix(key string) bool {
	for _, p := range boundaryMatchedKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// Substring match with version separators normalised, boundary families need a separator or end after the key
func catalogKeyMatches(key, modelLower string) bool {
	if hasBoundaryPrefix(key) {
		folded, err := hyphenatedGenerationRe.Replace(modelLower, "${1}${2}", -1, -1)
		if err == nil {
			modelLower = folded
		}
		if strings.HasSuffix(key, "-") {
			tail := lastPathSegment(modelLower)
			return regexp.MustCompile("^" + regexp.QuoteMeta(key) + "[a-z]").MatchString(tail)
		}
		return regexp.MustCompile(regexp.QuoteMeta(key) + `(?:[-:.@]|$)`).MatchString(modelLower)
	}
	return strings.Contains(modelLower, key) || strings.Contains(normalizeModelVersion(modelLower), normalizeModelVersion(key))
}

// First table entry whose key matches, longest key first with ties keeping table order
func longestKeyMatch(table []contextEntry, modelLower string) (contextEntry, bool) {
	sorted := append([]contextEntry{}, table...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].key) > len(sorted[j].key) })
	for _, e := range sorted {
		if catalogKeyMatches(e.key, modelLower) {
			return e, true
		}
	}
	return contextEntry{}, false
}

// Context length from an Ollama show payload, runtime num_ctx or GGUF training max
func ollamaShowContext(data map[string]any, ggufFirst bool, minimum int) (int, bool) {
	numCtx := func() []int {
		var out []int
		params, _ := data["parameters"].(string)
		for _, line := range strings.Split(params, "\n") {
			parts := strings.Fields(strings.TrimSpace(line))
			if strings.Contains(line, "num_ctx") && len(parts) >= 2 {
				if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
					out = append(out, n)
				}
			}
		}
		return out
	}
	gguf := func() []int {
		var out []int
		info, _ := data["model_info"].(map[string]any)
		for _, key := range sortedStringKeys(info) {
			if !strings.Contains(key, "context_length") {
				continue
			}
			switch v := info[key].(type) {
			case float64:
				out = append(out, int(v))
			case int:
				out = append(out, v)
			case int64:
				out = append(out, int(v))
			}
		}
		return out
	}
	readers := [][]int{numCtx(), gguf()}
	if ggufFirst {
		readers = [][]int{gguf(), numCtx()}
	}
	for _, values := range readers {
		for _, ctx := range values {
			if minimum <= 0 || ctx >= minimum {
				return ctx, true
			}
		}
	}
	return 0, false
}

type endpointScopedContext struct {
	host   string
	paths  map[string]bool
	models map[string]bool
	ctx    int
}

// Context confirmed for one provider endpoint only
var endpointScopedContexts = []endpointScopedContext{
	{"api.kimi.com", map[string]bool{"/coding": true, "/coding/v1": true}, map[string]bool{"k3": true, "kimi-k3": true, "kimi-k3-cot": true}, 1_048_576},
	{"integrate.api.nvidia.com", map[string]bool{"/v1": true}, map[string]bool{"deepseek-ai/deepseek-v4-pro": true}, 262_144},
}

// Endpoint scoped window on the canonical https host and path, zero elsewhere
func endpointScopedContextLength(model, baseURL string) (int, bool) {
	parsed, err := url.Parse(normalizeBaseURL(baseURL))
	if err != nil {
		return 0, false
	}
	port := parsed.Port()
	if strings.ToLower(parsed.Scheme) != "https" || (port != "" && port != "443") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, false
	}
	host, path, modelKey := strings.ToLower(parsed.Hostname()), strings.TrimRight(parsed.Path, "/"), strings.ToLower(strings.TrimSpace(model))
	for _, scoped := range endpointScopedContexts {
		if host == scoped.host && scoped.paths[path] && scoped.models[modelKey] {
			return scoped.ctx, true
		}
	}
	return 0, false
}

// Providers whose disk context cache must not short circuit probing
func skipPersistentContextCache(providerName string) bool {
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "lmstudio", "openai-codex":
		return true
	}
	return false
}

// Reports a request that travels the Codex Responses wire regardless of host
func isCodexRoute(ctx context.Context, providerName, baseURL string, customProviders []map[string]any) bool {
	if strings.ToLower(strings.TrimSpace(providerName)) == "openai-codex" {
		return true
	}
	if baseURL == "" {
		return false
	}
	cfg := metadataConfig(ctx)
	if cfg == nil {
		return false
	}
	return CustomProviderAPIMode(baseURL, customProviders, cfg) == string(ModeCodexResponses)
}

func saveUnlessSkipped(ctx context.Context, model, baseURL string, n int, providerName string) {
	if !skipPersistentContextCache(providerName) {
		SaveContextLength(ctx, model, baseURL, n, "")
	}
}

// Persists a probed local window only at or above the minimum
func maybeCacheLocalContextLength(ctx context.Context, model, baseURL string, length int) {
	if length >= MinimumContextLength {
		SaveContextLength(ctx, model, baseURL, length, "")
	}
}

// Live local probe that persists a positive result unless the provider opts out
func probeLocalContextLength(ctx context.Context, model, baseURL, apiKey, providerName string) int {
	localCtx := queryLocalContextLength(ctx, model, baseURL, apiKey)
	if localCtx <= 0 {
		return 0
	}
	if !skipPersistentContextCache(providerName) {
		maybeCacheLocalContextLength(ctx, model, baseURL, localCtx)
	}
	return localCtx
}

// Keeps the cached window unless a live local probe reports a different limit
func reconcileLocalCachedContextLength(ctx context.Context, model, baseURL string, cached int, apiKey string) int {
	live := queryLocalContextLength(ctx, model, baseURL, apiKey)
	if live <= 0 || live == cached {
		return cached
	}
	if live < MinimumContextLength {
		metadataLog.Info(fmt.Sprintf("Live local probe for %s@%s reports %s (< minimum %s); invalidating stale cache — agent init should reject", model, baseURL, formatWithCommas(live), formatWithCommas(MinimumContextLength)))
	} else {
		metadataLog.Info(fmt.Sprintf("Reconciling stale local cache entry %s@%s: %s -> %s (live probe)", model, baseURL, formatWithCommas(cached), formatWithCommas(live)))
	}
	invalidateCachedContextLength(ctx, model, baseURL)
	if live >= MinimumContextLength {
		maybeCacheLocalContextLength(ctx, model, baseURL, live)
	}
	return live
}

// IsLocalEndpoint is true for loopback, container DNS, unqualified, private, link local and Tailscale hosts
func IsLocalEndpoint(baseURL string) bool {
	parsed := parseProbeURL(baseURL, "http")
	if parsed == nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if metadataLocalHosts[host] || strings.HasSuffix(host, ".local") || (host != "" && !strings.Contains(host, ".") && !strings.Contains(host, ":")) {
		return true
	}
	for _, suffix := range metadataContainerLocalSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || (addr.Is4() && tailscaleCGNAT.Contains(addr)) {
			return true
		}
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	first, err1 := strconv.Atoi(parts[0])
	second, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return first == 10 || (first == 172 && second >= 16 && second <= 31) || (first == 192 && second == 168) || (first == 100 && second >= 64 && second <= 127)
}

var localhostHostRe = regexp2.MustCompile(`^(https?://)localhost(?=[:/]|$)`, regexp2.None)

// Rewrites a localhost host to the IPv4 loopback so dual stack hosts skip the IPv6 timeout
func localhostToIPv4(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	out, err := localhostHostRe.Replace(rawURL, "${1}127.0.0.1", -1, 1)
	if err != nil {
		return rawURL
	}
	return out
}

// Depth first walk over every nested mapping in a payload
func iterNestedDicts(value any, visit func(map[string]any) bool) bool {
	switch v := value.(type) {
	case map[string]any:
		if !visit(v) {
			return false
		}
		for _, key := range sortedStringKeys(v) {
			if !iterNestedDicts(v[key], visit) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !iterNestedDicts(item, visit) {
				return false
			}
		}
	}
	return true
}

// Integer within the plausible window bounds, rejecting bools and junk
func coerceReasonableInt(value any, minimum, maximum int) (int, bool) {
	var result int
	switch v := value.(type) {
	case bool:
		return 0, false
	case int:
		result = v
	case int64:
		result = int(v)
	case float64:
		result = int(v)
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(v), ",", ""))
		if err != nil {
			return 0, false
		}
		result = n
	default:
		return 0, false
	}
	if result < minimum || result > maximum {
		return 0, false
	}
	return result, true
}

func reasonableInt(value any) (int, bool) { return coerceReasonableInt(value, 1024, 10_000_000) }

// First plausible integer under any of the keys anywhere in the payload
func extractFirstInt(payload map[string]any, keys []string) (int, bool) {
	keyset := map[string]bool{}
	for _, k := range keys {
		keyset[strings.ToLower(k)] = true
	}
	found, ok := 0, false
	iterNestedDicts(payload, func(m map[string]any) bool {
		for _, key := range sortedStringKeys(m) {
			if keyset[strings.ToLower(key)] {
				if n, good := reasonableInt(m[key]); good {
					found, ok = n, true
					return false
				}
			}
		}
		return true
	})
	return found, ok
}

// Top level only context window read
func extractFlatContextLength(payload map[string]any) (int, bool) {
	for _, k := range contextLengthKeys {
		if n, ok := reasonableInt(payload[k]); ok {
			return n, true
		}
	}
	return 0, false
}

// Context window from a models object, window keys first and max_tokens last
func contextLengthFromModelPayload(payload map[string]any) (int, bool) {
	if payload == nil {
		return 0, false
	}
	if n, ok := extractFlatContextLength(payload); ok {
		return n, true
	}
	switch raw := payload["max_tokens"].(type) {
	case float64:
		if int(raw) > 0 {
			return int(raw), true
		}
	case int:
		if raw > 0 {
			return raw, true
		}
	case int64:
		if raw > 0 {
			return int(raw), true
		}
	}
	return 0, false
}

var pricingUnitDivisors = map[string]float64{
	"per_token": 1, "per_1k_tokens": 1_000, "per_thousand_tokens": 1_000,
	"per_1m_tokens": 1_000_000, "per_million_tokens": 1_000_000,
}

const perMillionQuoteMin = 0.001

var tokenRateFields = []string{"prompt", "completion", "cache_read", "cache_write"}

func floatOfAny(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// Rescales generic token rates to per token strings
func normalizeTokenRates(pricing map[string]any, unit any) map[string]any {
	rates := map[string]float64{}
	for _, key := range tokenRateFields {
		if v, ok := pricing[key]; ok {
			if f, good := floatOfAny(v); good {
				rates[key] = f
			}
		}
	}
	divisor, known := pricingUnitDivisors[strings.ToLower(strings.TrimSpace(stringOf(unit)))]
	if !known {
		divisor = 1
		for _, v := range rates {
			if v >= perMillionQuoteMin {
				divisor = 1_000_000
				break
			}
		}
	}
	if divisor != 1 {
		for key, value := range rates {
			pricing[key] = formatPyFloat(value / divisor)
		}
	}
	return pricing
}

// Shortest round trip decimal rendering of a float
func formatPyFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e16 && f > -1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.Contains(s, "e") {
		mant, exp, _ := strings.Cut(s, "e")
		expN, _ := strconv.Atoi(exp)
		if expN < 0 && expN >= -4 {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		if expN < 0 {
			return mant + "e-" + fmt.Sprintf("%02d", -expN)
		}
		return mant + "e+" + fmt.Sprintf("%02d", expN)
	}
	return s
}

// Per token pricing strings from a models payload, provider specific shapes first
func extractPricing(payload map[string]any) map[string]any {
	perToken := func(source map[string]any, fields [][2]string, scale func(float64) float64) map[string]any {
		out := map[string]any{}
		for _, pair := range fields {
			if v, ok := source[pair[1]]; ok && v != nil {
				if f, good := floatOfAny(v); good {
					out[pair[0]] = formatPyFloat(scale(f))
				}
			}
		}
		return out
	}
	novitaFields := [][2]string{{"prompt", "input_token_price_per_m"}, {"completion", "output_token_price_per_m"}}
	for _, pair := range novitaFields {
		if v, ok := payload[pair[1]]; ok && v != nil {
			return perToken(payload, novitaFields, func(v float64) float64 { return v / 10_000 / 1_000_000 })
		}
	}
	if metadata, ok := payload["metadata"].(map[string]any); ok {
		if dp, ok := metadata["pricing"].(map[string]any); ok {
			deepinfraFields := [][2]string{{"prompt", "input_tokens"}, {"completion", "output_tokens"}, {"cache_read", "cache_read_tokens"}}
			for _, pair := range deepinfraFields {
				if _, has := dp[pair[1]]; has {
					return perToken(dp, deepinfraFields, func(v float64) float64 { return v / 1_000_000 })
				}
			}
		}
	}
	aliasMap := []struct {
		target  string
		aliases []string
	}{
		{"prompt", []string{"prompt", "input", "input_cost_per_token", "prompt_token_cost"}},
		{"completion", []string{"completion", "output", "output_cost_per_token", "completion_token_cost"}},
		{"request", []string{"request", "request_cost"}},
		{"cache_read", []string{"cache_read", "cached_prompt", "input_cache_read", "cache_read_cost_per_token"}},
		{"cache_write", []string{"cache_write", "cache_creation", "input_cache_write", "cache_write_cost_per_token"}},
	}
	result := map[string]any{}
	iterNestedDicts(payload, func(m map[string]any) bool {
		normalized := map[string]any{}
		for k, v := range m {
			normalized[strings.ToLower(k)] = v
		}
		pricing := map[string]any{}
		for _, entry := range aliasMap {
			for _, alias := range entry.aliases {
				v, has := normalized[alias]
				if has && v != nil && v != "" {
					pricing[entry.target] = v
					break
				}
			}
		}
		if len(pricing) > 0 {
			result = normalizeTokenRates(pricing, normalized["unit"])
			return false
		}
		return true
	})
	return result
}

// Stores an entry under the id and under its bare slug when vendor prefixed
func addModelAliases(cache map[string]map[string]any, modelID string, entry map[string]any) {
	cache[modelID] = entry
	if i := strings.Index(modelID, "/"); i >= 0 {
		if _, has := cache[modelID[i+1:]]; !has {
			cache[modelID[i+1:]] = entry
		}
	}
}
