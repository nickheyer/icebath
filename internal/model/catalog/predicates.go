package catalog

import (
	"context"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	DefaultFallbackContext    = 256_000
	MinimumContextLength      = 64_000
	CodexContextVariantSuffix = "-900k"
	codexStaleAdvertised      = 272_000
)

type windowRow struct {
	key    string
	window int
}

var defaultContextLengths = []windowRow{
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

var defaultContextByLength = longestFirst(defaultContextLengths)

var codexOAuthFallback = longestFirst([]windowRow{
	{"gpt-6-astra", 272_000}, {"gpt-6.1-sol", 272_000}, {"gpt-6-sol", 272_000}, {"gpt-6-luna", 272_000},
	{"gpt-5.1-codex-max", 272_000}, {"gpt-5.1-codex-mini", 272_000}, {"gpt-5.3-codex", 272_000},
	{"gpt-5.3-codex-spark", 128_000}, {"gpt-5.2-codex", 272_000}, {"gpt-5.4-mini", 272_000},
	{"gpt-5.6-sol", 272_000}, {"gpt-5.6-terra", 272_000}, {"gpt-5.6-luna", 272_000}, {"gpt-daybreak-blue-latest", 272_000},
	{"gpt-5.5", 272_000}, {"gpt-5.4", 272_000}, {"gpt-5.2", 272_000}, {"gpt-5", 272_000},
})

var codexVerifiedPrefixes = []windowRow{{"gpt-5.6", 900_000}, {"gpt-6-sol", 900_000}, {"gpt-6-luna", 900_000}}

var codexVerifiedExact = map[string]int{
	"gpt-5.4": 900_000, "gpt-daybreak-blue-latest": 900_000, "gpt-6-astra": 900_000, "gpt-6.1-sol": 900_000,
}

var codex900kSnapshotBases = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna"}

var codex900kEligibleBases = append(slices.Clone(codex900kSnapshotBases), "gpt-5.4", "gpt-daybreak-blue-latest", "gpt-6-astra", "gpt-6.1-sol")

var codex900kSnapshot = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

var preCatalogStaleKeys = []string{
	"minimax-m3", "muse-spark-1.3", "muse-spark", "grok-4.3", "grok-4.6", "grok-4-fast", "grok-4.20", "qwen3.6-plus",
	"deepseek-flash", "deepseek-v4.1-flash", "deepseek-v4-flash", "deepseek-v4-pro", "deepseek-chat", "deepseek-reasoner",
}

var grokEffortPrefixes = []string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"}

var openAINonReasoning = regexp.MustCompile(`^(?:ft:)?(?:gpt-3\.5|gpt-4|chatgpt-)`)

var anthropicFastModels = []string{"claude-opus-4-8", "claude-opus-5", "claude-opus-5-5"}

var (
	digitDotDigit   = regexp.MustCompile(`(\d)\.(\d)`)
	datedSuffix     = regexp.MustCompile(`-\d{8}$`)
	ollamaTag       = regexp.MustCompile(`(?i)^(\d+\.?\d*b|latest|stable|q\d|fp?\d|instruct|chat|coder|vision|text)`)
	hyphenatedSolar = regexp.MustCompile(`(solar-[a-z]+)-(\d{1,2})([-:.@]|$)`)
	localhostURL    = regexp.MustCompile(`^(https?://)localhost([:/]|$)`)
)

var contextLengthKeys = []string{
	"context_length", "context_window", "context_size", "max_context_length", "max_position_embeddings",
	"max_model_len", "max_input_tokens", "max_sequence_length", "max_seq_len", "n_ctx_train", "n_ctx", "ctx_size",
}

var maxCompletionKeys = []string{"max_completion_tokens", "max_output_tokens", "max_tokens"}

var localHosts = []string{"localhost", "127.0.0.1", "::1", "0.0.0.0"}

var containerLocalSuffixes = []string{".docker.internal", ".containers.internal", ".lima.internal"}

var openRouterVariantSuffixes = []string{"nitro", "floor", "exacto", "online"}

type hostRow struct {
	part     string
	provider string
}

var urlProviders = []hostRow{
	{"api.openai.com", "openai"}, {"chatgpt.com", "openai"}, {"api.anthropic.com", "anthropic"},
	{"api.z.ai", "zai"}, {"open.bigmodel.cn", "zai"},
	{"api.moonshot.ai", "kimi-coding"}, {"api.moonshot.cn", "kimi-coding-cn"}, {"api.kimi.com", "kimi-coding"},
	{"api.stepfun.ai", "stepfun"}, {"api.stepfun.com", "stepfun"}, {"api.arcee.ai", "arcee"}, {"api.minimax", "minimax"},
	{"dashscope.aliyuncs.com", "alibaba"}, {"dashscope-intl.aliyuncs.com", "alibaba"}, {"portal.qwen.ai", "qwen-oauth"},
	{"openrouter.ai", "openrouter"}, {"generativelanguage.googleapis.com", "gemini"},
	{"inference-api.icebath.app", "portal"}, {"api.deepseek.com", "deepseek"},
	{"api.fireworks.ai", "fireworks"}, {"opencode.ai", "opencode-go"}, {"api.x.ai", "xai"},
	{"integrate.api.nvidia.com", "nvidia"}, {"api.xiaomimimo.com", "xiaomi"}, {"xiaomimimo.com", "xiaomi"},
	{"api.gmi-serving.com", "gmi"}, {"api.novita.ai", "novita"},
	{"tokenhub.tencentmaas.com", "tencent-tokenhub"}, {"api.lkeap.cloud.tencent.com", "tencent-tokenplan"},
	{"ollama.com", "ollama-cloud"},
}

type scopedWindow struct {
	host   string
	paths  []string
	models []string
	window int
}

var endpointScopedContext = []scopedWindow{
	{"api.kimi.com", []string{"/coding", "/coding/v1"}, []string{"k3", "kimi-k3", "kimi-k3-cot"}, 1_048_576},
	{"integrate.api.nvidia.com", []string{"/v1"}, []string{"deepseek-ai/deepseek-v4-pro"}, 262_144},
}

var privateNetworks = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/29", "192.0.0.170/31",
	"192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"255.255.255.255/32", "::1/128", "::/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
	"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10",
)

var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

func mustPrefixes(texts ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(texts))
	for _, text := range texts {
		out = append(out, netip.MustParsePrefix(text))
	}
	return out
}

func longestFirst(rows []windowRow) []windowRow {
	sorted := slices.Clone(rows)
	slices.SortStableFunc(sorted, func(a, b windowRow) int { return len(b.key) - len(a.key) })
	return sorted
}

func lastSegment(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

func GrokSupportsReasoningEffort(model string) bool {
	name := lastSegment(strings.ToLower(strings.TrimSpace(model)))
	return name != "" && slices.ContainsFunc(grokEffortPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

func OpenAIModelRejectsReasoning(model string) bool {
	name := lastSegment(strings.ToLower(strings.TrimSpace(model)))
	loc := openAINonReasoning.FindStringIndex(name)
	if loc == nil {
		return false
	}
	matched := name[loc[0]:loc[1]]
	if strings.HasSuffix(matched, "gpt-4") && loc[1] < len(name) && name[loc[1]] >= '0' && name[loc[1]] <= '9' {
		return false
	}
	return true
}

func IsGrok46Family(model string) bool {
	name := lastSegment(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-"))
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

func IsAnthropicFastModeModel(model string) bool {
	name, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(model)), ":")
	name = lastSegment(name)
	name = digitDotDigit.ReplaceAllString(name, "${1}-${2}")
	return slices.Contains(anthropicFastModels, datedSuffix.ReplaceAllString(name, ""))
}

func ModelSuggestsMiniMaxM3(model string) bool {
	return strings.Contains(strings.ToLower(model), "minimax-m3")
}

func suggestsKimi(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "kimi") || strings.Contains(lower, "moonshot")
}

func suggestsMiniMax(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "minimax") || strings.Contains(lower, "minimaxai/")
}

func suggestsStale32K(model string) bool { return suggestsKimi(model) || suggestsMiniMax(model) }

func ModelIDMatches(candidate, lookup string) bool {
	if candidate == lookup {
		return true
	}
	i := strings.LastIndex(candidate, "/")
	return i >= 0 && candidate[i+1:] == lookup
}

func BareCodexSlug(model string) string {
	return lastSegment(strings.ToLower(strings.TrimSpace(model)))
}

func IsCodex900kBase(model string) bool {
	slug := BareCodexSlug(model)
	if slug == "" || strings.HasSuffix(slug, CodexContextVariantSuffix) {
		return false
	}
	if slices.Contains(codex900kEligibleBases, slug) {
		return true
	}
	return slices.ContainsFunc(codex900kSnapshotBases, func(base string) bool {
		return strings.HasPrefix(slug, base+"-") && codex900kSnapshot.MatchString(slug[len(base)+1:])
	})
}

func codexVariantBase(model string) (string, bool) {
	slug := BareCodexSlug(model)
	if !strings.HasSuffix(slug, CodexContextVariantSuffix) {
		return "", false
	}
	base := slug[:len(slug)-len(CodexContextVariantSuffix)]
	return base, IsCodex900kBase(base)
}

func IsCodexContextVariant(model string) bool {
	_, ok := codexVariantBase(model)
	return ok
}

func StripCodexContextVariantSuffix(model string) string {
	raw := strings.TrimSpace(model)
	if _, ok := codexVariantBase(raw); ok {
		return raw[:len(raw)-len(CodexContextVariantSuffix)]
	}
	return raw
}

func verifiedCodexWindow(modelBare string) (int, bool) {
	base, ok := codexVariantBase(modelBare)
	if !ok {
		return 0, false
	}
	if exact, ok := codexVerifiedExact[base]; ok {
		return exact, true
	}
	for _, row := range codexVerifiedPrefixes {
		if base == row.key || strings.HasPrefix(base, row.key+"-") || strings.HasPrefix(base, row.key+".") {
			return row.window, true
		}
	}
	return 0, false
}

func OpenRouterVariantBase(model string) (string, bool) {
	i := strings.LastIndex(model, ":")
	if i <= 0 {
		return "", false
	}
	if slices.Contains(openRouterVariantSuffixes, strings.ToLower(model[i+1:])) {
		return model[:i], true
	}
	return "", false
}

func normalizeBaseURL(baseURL string) string {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func parseBaseURL(baseURL, scheme string) *url.URL {
	normalized := normalizeBaseURL(baseURL)
	if normalized == "" {
		return nil
	}
	if !strings.Contains(normalized, "://") {
		normalized = scheme + "://" + normalized
	}
	u, err := url.Parse(normalized)
	if err != nil {
		return nil
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return nil
		}
	}
	return u
}

func endpointHostKey(baseURL string) (string, bool) {
	u := parseBaseURL(baseURL, "http")
	if u == nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port, true
}

func IsLocalEndpoint(baseURL string) bool {
	normalized := normalizeBaseURL(baseURL)
	if normalized == "" {
		return false
	}
	u := parseBaseURL(baseURL, "http")
	if u == nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if slices.Contains(localHosts, host) || strings.HasSuffix(host, ".local") ||
		slices.ContainsFunc(containerLocalSuffixes, func(s string) bool { return strings.HasSuffix(host, s) }) ||
		(host != "" && !strings.Contains(host, ".") && !strings.Contains(host, ":")) {
		return true
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		plain := addr.WithZone("")
		if plain.IsLoopback() || plain.IsLinkLocalUnicast() || (plain.Is4() && tailscaleCGNAT.Contains(plain)) ||
			slices.ContainsFunc(privateNetworks, func(p netip.Prefix) bool { return p.Contains(plain) }) {
			return true
		}
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	first, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	second, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return first == 10 || (first == 172 && second >= 16 && second <= 31) || (first == 192 && second == 168) || (first == 100 && second >= 64 && second <= 127)
}

func localhostToIPv4(rawURL string) string {
	return localhostURL.ReplaceAllString(rawURL, "${1}127.0.0.1${2}")
}

func lmStudioServerRoot(baseURL string) string {
	root := normalizeBaseURL(baseURL)
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			return strings.TrimRight(root[:len(root)-len(suffix)], "/")
		}
	}
	return root
}

func serverRoot(baseURL string) string {
	return strings.TrimSuffix(localhostToIPv4(strings.TrimRight(baseURL, "/")), "/v1")
}

func normalizeModelVersion(model string) string {
	return strings.ReplaceAll(model, ".", "-")
}

func boundaryByte(b byte) bool { return b == '-' || b == ':' || b == '.' || b == '@' }

func catalogKeyMatches(key, modelLower string) bool {
	if strings.HasPrefix(key, "solar-") {
		modelLower = hyphenatedSolar.ReplaceAllString(modelLower, "${1}${2}${3}")
		if strings.HasSuffix(key, "-") {
			tail := lastSegment(modelLower)
			return strings.HasPrefix(tail, key) && len(tail) > len(key) && tail[len(key)] >= 'a' && tail[len(key)] <= 'z'
		}
		for from := 0; ; {
			i := strings.Index(modelLower[from:], key)
			if i < 0 {
				return false
			}
			end := from + i + len(key)
			if end == len(modelLower) || boundaryByte(modelLower[end]) {
				return true
			}
			from += i + 1
		}
	}
	return strings.Contains(modelLower, key) || strings.Contains(normalizeModelVersion(modelLower), normalizeModelVersion(key))
}

func longestKeyMatch(sorted []windowRow, modelLower string) (windowRow, bool) {
	for _, row := range sorted {
		if catalogKeyMatches(row.key, modelLower) {
			return row, true
		}
	}
	return windowRow{}, false
}

func DefaultContextLength(model string) (int, bool) {
	row, ok := longestKeyMatch(defaultContextByLength, strings.ToLower(model))
	return row.window, ok
}

func stalePreCatalogEntry(model string, cached int) bool {
	modelLower := strings.ToLower(model)
	var matches []windowRow
	for _, row := range defaultContextLengths {
		if catalogKeyMatches(row.key, modelLower) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return false
	}
	specific := matches[0]
	for _, row := range matches[1:] {
		if len(row.key) > len(specific.key) {
			specific = row
		}
	}
	if !slices.Contains(preCatalogStaleKeys, specific.key) || cached >= specific.window {
		return false
	}
	ceiling, found := 0, false
	for _, row := range matches {
		if len(row.key) < len(specific.key) && (!found || row.window > ceiling) {
			ceiling, found = row.window, true
		}
	}
	if !found {
		ceiling = DefaultFallbackContext
	}
	return cached <= ceiling
}

func endpointScopedContextLength(model, baseURL string) (int, bool) {
	u, err := url.Parse(normalizeBaseURL(baseURL))
	if err != nil {
		return 0, false
	}
	port := u.Port()
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			return 0, false
		}
	}
	if strings.ToLower(u.Scheme) != "https" || (port != "" && port != "443") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return 0, false
	}
	host, path, modelKey := strings.ToLower(u.Hostname()), strings.TrimRight(u.Path, "/"), strings.ToLower(strings.TrimSpace(model))
	for _, row := range endpointScopedContext {
		if host == row.host && slices.Contains(row.paths, path) && slices.Contains(row.models, modelKey) {
			return row.window, true
		}
	}
	return 0, false
}

func (c *Catalog) StripProviderPrefix(ctx context.Context, model string) string {
	if !strings.Contains(model, ":") || strings.HasPrefix(model, "http") {
		return model
	}
	ctx = c.scoped(ctx)
	prefix, suffix, _ := strings.Cut(model, ":")
	if _, ok := c.reg.Get(ctx, strings.ToLower(strings.TrimSpace(prefix))); ok && !ollamaTag.MatchString(strings.TrimSpace(suffix)) {
		return suffix
	}
	return model
}

func (c *Catalog) InferProviderFromURL(ctx context.Context, baseURL string) (string, bool) {
	u := parseBaseURL(baseURL, "https")
	if u == nil {
		return "", false
	}
	ctx = c.scoped(ctx)
	netloc := u.Host
	if u.User != nil {
		netloc = u.User.String() + "@" + netloc
	}
	host := strings.ToLower(netloc)
	if host == "" {
		host = strings.ToLower(u.Path)
	}
	if p, ok := c.reg.ForHost(ctx, u.String(), true); ok {
		return p.Spec().Name, true
	}
	for _, row := range urlProviders {
		if strings.Contains(host, row.part) {
			return row.provider, true
		}
	}
	for _, p := range c.reg.All(ctx) {
		spec := p.Spec()
		claimed := spec.Host()
		if claimed == "" || slices.ContainsFunc(urlProviders, func(r hostRow) bool { return r.part == claimed }) {
			continue
		}
		if strings.Contains(host, claimed) {
			return spec.Name, true
		}
	}
	return "", false
}

func (c *Catalog) isKnownProviderBaseURL(ctx context.Context, baseURL string) bool {
	_, ok := c.InferProviderFromURL(ctx, baseURL)
	return ok
}

func isCustomEndpoint(baseURL string) bool {
	normalized := normalizeBaseURL(baseURL)
	return normalized != "" && !providers.HostMatches(normalized, "openrouter.ai")
}

func (c *Catalog) stripOpenRouterVariant(ctx context.Context, model, baseURL, provider string) string {
	if model == "" {
		return model
	}
	routed := strings.ToLower(strings.TrimSpace(provider)) == "openrouter"
	if !routed && baseURL != "" {
		inferred, ok := c.InferProviderFromURL(ctx, baseURL)
		routed = ok && inferred == "openrouter"
	}
	if !routed {
		return model
	}
	base, ok := OpenRouterVariantBase(model)
	if !ok {
		return model
	}
	slog.Debug("resolving context length for an OpenRouter routing variant through its base id", "model", model, "base", base)
	return base
}
