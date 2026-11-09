package provider

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Set by the first stale custom base URL warning so later resolutions stay quiet
var auxStaleBaseURLWarned atomic.Bool

var auxLocalServerAliases = map[string]string{
	"ollama": "custom", "vllm": "custom", "llamacpp": "custom", "llama.cpp": "custom", "llama-cpp": "custom",
}

type auxAliasTable struct {
	generation int64
	table      map[string]string
}

var (
	auxAliasTablesMu sync.Mutex
	auxAliasTables   = map[string]auxAliasTable{}
)

// Auth provider aliases extended with the aliases every provider plugin of the bound home declares
func authPluginAliases(ctx context.Context) map[string]string {
	aliases := make(map[string]string, len(authProviderAliases))
	for k, v := range authProviderAliases {
		aliases[k] = v
	}
	for _, pp := range ListProviders(ctx) {
		for _, alias := range pp.Aliases {
			if _, ok := aliases[alias]; !ok {
				aliases[alias] = pp.Name
			}
		}
	}
	return aliases
}

// Alias table of the bound home rebuilt whenever its registry generation moves
func auxProviderAliasTable(ctx context.Context) map[string]string {
	h, generation := BoundRegistryKey(ctx)
	auxAliasTablesMu.Lock()
	cached, ok := auxAliasTables[h]
	auxAliasTablesMu.Unlock()
	if ok && cached.generation == generation {
		return cached.table
	}
	table := make(map[string]string, len(auxLocalServerAliases))
	for k, v := range auxLocalServerAliases {
		table[k] = v
	}
	for k, v := range authPluginAliases(ctx) {
		table[k] = v
	}
	auxAliasTablesMu.Lock()
	auxAliasTables[h] = auxAliasTable{generation: generation, table: table}
	auxAliasTablesMu.Unlock()
	return table
}

// Canonical auxiliary provider id with custom prefixes, codex, main and every alias collapsed
func auxNormalizeAuxProvider(ctx context.Context, provider string) string {
	if provider == "" {
		provider = "auto"
	}
	normalized := pystr.Lower(pystr.Strip(provider))
	if strings.HasPrefix(normalized, "custom:") {
		suffix := pystr.Strip(normalized[len("custom:"):])
		if suffix == "" {
			return "custom"
		}
		normalized = suffix
	}
	if normalized == "codex" {
		return "openai-codex"
	}
	if normalized == "main" {
		mainProv := pystr.Lower(pystr.Strip(auxReadMainProvider(ctx)))
		if mainProv == "" || mainProv == "auto" || mainProv == "main" {
			return "custom"
		}
		normalized = mainProv
	}
	if canon, ok := auxProviderAliasTable(ctx)[normalized]; ok {
		return canon
	}
	return normalized
}

// Canonical auxiliary provider slug of a configured provider name
func NormalizeAuxProvider(ctx context.Context, provider string) string {
	return auxNormalizeAuxProvider(ctx, provider)
}

// Lowercased model slug with any vendor prefix dropped
func bareModel(model string) string {
	m := pystr.Lower(pystr.Strip(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		return m[i+1:]
	}
	return m
}

// Bare slug of a kwargs model value, an AttributeError for a truthy value that is not a string
func auxBareModelOf(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return bareModel(""), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(v))
	}
	return bareModel(s), nil
}

// True for Kimi and Moonshot models whose temperature the server manages
func isKimiModel(model string) bool {
	bare := bareModel(model)
	return strings.HasPrefix(bare, "kimi-") || bare == "kimi"
}

// True for Arcee Trinity Large Thinking direct or through an aggregator
func auxIsArceeTrinityThinking(model string) bool {
	return bareModel(model) == "trinity-large-thinking"
}

const (
	auxCodexGPT54GPT55CompactionThreshold = 0.85
	auxCodexSparkCompactionThreshold      = 0.70
)

// True for the 272K capped gpt-5.4 to 5.6, gpt-6 Sol, Luna and Astra families on the Codex OAuth route
func auxIsCodexGPT54OrGPT55(model, provider string) bool {
	bare, ok := auxCodexRouteBareModel(model, provider)
	if !ok || IsCodexContextVariant(bare) {
		return false
	}
	if strings.Contains(bare, "astra") {
		return !strings.Contains(bare, "900k")
	}
	if bare == "gpt-daybreak-blue-latest" {
		return true
	}
	for _, fam := range []string{"gpt-5.4", "gpt-5.5", "gpt-5.6", "gpt-6-sol", "gpt-6.1-sol", "gpt-6-luna"} {
		if bare == fam || strings.HasPrefix(bare, fam+"-") || strings.HasPrefix(bare, fam+".") {
			return true
		}
	}
	return false
}

// True for the 272K capped gpt-5.4 to 5.6 and gpt-6 families on the Codex OAuth route
func IsCodexGPT54OrGPT55(model, provider string) bool { return auxIsCodexGPT54OrGPT55(model, provider) }

// Bare model slug when the provider is the Codex OAuth route, false otherwise
func auxCodexRouteBareModel(model, provider string) (string, bool) {
	if pystr.Lower(pystr.Strip(provider)) != "openai-codex" {
		return "", false
	}
	return bareModel(model), true
}

// True for gpt-5.3-codex-spark on the Codex OAuth route
func auxIsCodexSpark(model, provider string) bool {
	bare, ok := auxCodexRouteBareModel(model, provider)
	return ok && bare == "gpt-5.3-codex-spark"
}

// True for gpt-5.3-codex-spark on the Codex OAuth route
func IsCodexSpark(model, provider string) bool { return auxIsCodexSpark(model, provider) }

// True for OpenAI reasoning families that reject any temperature but the default
func isOpenAIDefaultTemperatureOnly(model string) bool {
	bare := bareModel(model)
	return (strings.HasPrefix(bare, "gpt-5") || strings.HasPrefix(bare, "o1") || strings.HasPrefix(bare, "o3") ||
		strings.HasPrefix(bare, "o4")) && !strings.HasPrefix(bare, "gpt-5-chat")
}

type auxTemperatureRoute struct{ route, model string }

var (
	auxTemperatureRejectedMu     sync.Mutex
	auxTemperatureRejectedRoutes = map[auxTemperatureRoute]bool{}
)

// RememberTemperatureRejection omits temperature from the next request of the route and rejected model
func RememberTemperatureRejection(provider, baseURL string, rejectedKwargs map[string]any, _ error) error {
	route, err := auxRouteKey(provider, baseURL)
	if err != nil {
		return err
	}
	model, err := auxBareModelOf(rejectedKwargs["model"])
	if err != nil {
		return err
	}
	auxTemperatureRejectedMu.Lock()
	auxTemperatureRejectedRoutes[auxTemperatureRoute{route, model}] = true
	auxTemperatureRejectedMu.Unlock()
	return nil
}

// Forgets every remembered temperature rejection as a fresh process starts
func auxResetTemperatureRejections() {
	auxTemperatureRejectedMu.Lock()
	auxTemperatureRejectedRoutes = map[auxTemperatureRoute]bool{}
	auxTemperatureRejectedMu.Unlock()
}

// Temperature policy as omit, a fixed value, or neither when the caller's value stands
func auxFixedTemperatureForModel(model, baseURL, provider string) (omit bool, fixed *float64, err error) {
	if isKimiModel(model) {
		slog.Debug("Omitting temperature for Kimi model " + pyrepr.Repr(model) + " (server-managed)")
		return true, nil, nil
	}
	if isOpenAIDefaultTemperatureOnly(model) || geminiSamplingDeprecated(bareModel(model)) {
		slog.Debug("Omitting temperature for " + pyrepr.Repr(model) + " (default-only, or Gemini 3.6+ ignores sampling)")
		return true, nil, nil
	}
	route, err := auxRouteKey(provider, baseURL)
	if err != nil {
		return false, nil, err
	}
	auxTemperatureRejectedMu.Lock()
	rejected := auxTemperatureRejectedRoutes[auxTemperatureRoute{route, bareModel(model)}]
	auxTemperatureRejectedMu.Unlock()
	if rejected {
		slog.Debug("Omitting temperature for " + pyrepr.Repr(model) + " (route rejected it earlier)")
		return true, nil, nil
	}
	if auxIsArceeTrinityThinking(model) {
		v := 0.5
		return false, &v, nil
	}
	return false, nil, nil
}

// FixedTemperatureForModel is the omit flag or fixed temperature a route sends for the model
func FixedTemperatureForModel(model, baseURL, provider string) (omit bool, fixed *float64, err error) {
	return auxFixedTemperatureForModel(model, baseURL, provider)
}

// Per model and route compaction threshold override, false when the user's value stands
func auxCompressionThresholdForModel(model, provider string, allowCodexGPT55Autoraise bool) (float64, bool) {
	if auxIsArceeTrinityThinking(model) {
		return 0.75, true
	}
	if allowCodexGPT55Autoraise && auxIsCodexGPT54OrGPT55(model, provider) {
		return auxCodexGPT54GPT55CompactionThreshold, true
	}
	if auxIsCodexSpark(model, provider) {
		return auxCodexSparkCompactionThreshold, true
	}
	return 0, false
}

// Per model and route compaction threshold override, false when the user's value stands
func CompressionThresholdForModel(model, provider string, allowCodexGPT55Autoraise bool) (float64, bool) {
	return auxCompressionThresholdForModel(model, provider, allowCodexGPT55Autoraise)
}

var auxFastModelFamilies = []string{
	"gpt-mini-latest", "gpt-nano-latest", "claude-haiku-latest", "gemini-flash-latest",
	"gpt-5.4-nano", "gpt-5.4-mini", "gpt-5-mini", "haiku-4.5", "gemini-3.6-flash", "flash-lite",
	"-nano", "-mini", "-flash", "haiku",
}

var auxFastModelExclude = []string{
	"thinking", "reason", "-r1", "minilm", ":batch", ":free",
	"o1-", "o3-", "o4-", "codex", "audio", "-vl", "embed",
	"-tts", "-transcribe", "-realtime", "-image", "-search-preview",
}

type auxRecencyPart struct {
	kind int
	num  float64
	text string
}

var auxRecencySplitRe = regexp.MustCompile(`\p{Nd}+(?:\.\p{Nd}+)?`)

// Sort key comparing digit runs numerically so a family's newest release sorts last
func auxModelRecencyKey(modelID string) []auxRecencyPart {
	lowered := pystr.Lower(modelID)
	var parts []auxRecencyPart
	pos := 0
	for _, m := range auxRecencySplitRe.FindAllStringIndex(lowered, -1) {
		if text := lowered[pos:m[0]]; text != "" {
			parts = append(parts, auxRecencyPart{kind: 0, text: text})
		}
		parts = append(parts, auxRecencyPart{kind: 1, num: auxPyFloatDigits(lowered[m[0]:m[1]])})
		pos = m[1]
	}
	if tail := lowered[pos:]; tail != "" {
		parts = append(parts, auxRecencyPart{kind: 0, text: tail})
	}
	return parts
}

// Python float of a run of Unicode decimal digits with at most one dot
func auxPyFloatDigits(run string) float64 {
	var b strings.Builder
	for _, r := range run {
		if d, ok := pystr.DecimalValue(r); ok {
			b.WriteByte(byte('0' + d))
		} else {
			b.WriteRune(r)
		}
	}
	f, _ := strconv.ParseFloat(b.String(), 64)
	return f
}

// Python tuple ordering of two recency keys
func auxCompareRecency(a, b []auxRecencyPart) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		switch {
		case x.kind != y.kind:
			return x.kind - y.kind
		case x.num < y.num:
			return -1
		case x.num > y.num:
			return 1
		case x.text != y.text:
			return strings.Compare(x.text, y.text)
		}
	}
	return len(a) - len(b)
}

var (
	auxResolveAPIKeyProviderCredentialsFn = func(ctx context.Context, provider string) (apiKeyCreds, error) {
		return modelsResolver(ctx).apiKeyProviderCredentials(provider)
	}
	auxFetchModelsWithPricingFn = FetchModelsWithPricing
	auxFastModelFromCatalogFn   = auxFastModelFromCatalog
)

// Newest fast family match from the provider's live catalog, empty when none or the catalog is unreachable
func auxFastModelFromCatalog(ctx context.Context, providerID string) string {
	isNous := pystr.Lower(pystr.Strip(providerID)) == "nous"
	apiKey, baseURL := "", ""
	if creds, err := auxResolveAPIKeyProviderCredentialsFn(ctx, providerID); err != nil {
		slog.Debug("No credentials for "+providerID+" catalog", "err", err)
	} else {
		apiKey, baseURL = pystr.Strip(creds.APIKey), pystr.Strip(creds.BaseURL)
	}
	if apiKey == "" && isNous {
		apiKey, baseURL = resolveNousPricingCredentials(ctx)
	}
	if baseURL == "" {
		if profile, ok := GetProviderProfile(ctx, providerID); ok {
			baseURL = profile.BaseURL
		}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return ""
	}
	opts := PricingFetchOptions{APIKey: apiKey, BaseURL: strings.TrimSuffix(baseURL, "/v1"), Timeout: 3 * time.Second}
	if isNous {
		opts.IncludeSaleOriginal, opts.CacheTTL = true, NousCatalogTTL
	}
	catalog := auxFetchModelsWithPricingFn(opts)
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	keys := make(map[string][]auxRecencyPart, len(ids))
	for _, id := range ids {
		keys[id] = auxModelRecencyKey(id)
	}
	sort.SliceStable(ids, func(i, j int) bool { return auxCompareRecency(keys[ids[i]], keys[ids[j]]) > 0 })
	if isNous {
		allowed, _ := nousPolicyAllowedIDsFn(ctx, false)
		ids = RestrictToNousPolicy(ids, allowed, false)
	}
	for _, family := range auxFastModelFamilies {
		for _, modelID := range ids {
			lowered := pystr.Lower(modelID)
			if strings.Contains(lowered, family) && !auxContainsAny(lowered, auxFastModelExclude) {
				return modelID
			}
		}
	}
	return ""
}

// Reports whether any needle is a substring of the text
func auxContainsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Cheap auxiliary model of a provider through its profile, the curated default and the legacy table
func auxGetAuxModelForProvider(ctx context.Context, providerID string) string {
	return auxAuxModelForProvider(ctx, providerID, false)
}

// Provider aux model lookup, swapped by tests the way hermes tests patch the module function
var auxGetAuxModelForProviderFn = auxGetAuxModelForProvider

// Auxiliary model ladder whose live catalog fast tier runs only when the caller opts in
func auxAuxModelForProvider(ctx context.Context, providerID string, preferFast bool) string {
	profile, hasProfile := GetProviderProfile(ctx, providerID)
	picked := ""
	if preferFast {
		picked = auxFastModelFromCatalogFn(ctx, providerID)
		if picked == "" && hasProfile {
			picked = profile.ResolveAuxModel(ctx, false)
		}
	}
	if picked == "" && hasProfile && profile.DefaultAuxModel != "" {
		picked = profile.DefaultAuxModel
	}
	if picked == "" {
		picked = auxAPIKeyProviderAuxModelsFallback[providerID]
	}
	if picked != "" && pystr.Lower(pystr.Strip(providerID)) == "nous" {
		if allowed, _ := nousPolicyAllowedIDsFn(ctx, false); len(allowed) > 0 && len(RestrictToNousPolicy([]string{picked}, allowed, false)) == 0 {
			return ""
		}
	}
	return picked
}

// Cheap auxiliary model of a provider, the live catalog fast tier first when preferFast is set
func AuxModelForProvider(ctx context.Context, providerID string, preferFast bool) string {
	return auxAuxModelForProvider(ctx, providerID, preferFast)
}

var auxAPIKeyProviderAuxModelsFallback = map[string]string{
	"gemini": "gemini-3.6-flash", "zai": "glm-4.5-flash", "kimi-coding": "kimi-k2-turbo-preview",
	"stepfun": "step-3.5-flash", "kimi-coding-cn": "kimi-k2-turbo-preview",
	"gmi": "google/gemini-3.1-flash-lite-preview", "anthropic": "claude-haiku-4-5-20251001",
	"ai-gateway": "google/gemini-3-flash", "opencode-zen": "gemini-3-flash", "opencode-go": "glm-5",
	"kilocode": "google/gemini-3.6-flash", "ollama-cloud": "nemotron-3-nano:30b",
	"tencent-tokenhub": "hy4-preview", "tencent-tokenplan": "hy4-preview",
}

var auxFastModelTasks = map[string]bool{"title_generation": true}

// Whether an eligible task opted into fast model routing through auxiliary.<task>.prefer_fast_model
func auxTaskPrefersFastModel(ctx context.Context, task any) (bool, error) {
	if _, isStr := task.(string); !isStr {
		if _, isAuth := task.(AuthoritativeAuxiliaryTask); !isAuth {
			return false, nil
		}
	}
	if !auxFastModelTasks[auxTaskName(task)] {
		return false, nil
	}
	config, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil {
		return false, err
	}
	v, ok := config["prefer_fast_model"]
	return ok && v != nil && isTruthyValue(v), nil
}

// Task config read from the session scope or the bound home's readonly config.yaml
func auxGetAuxiliaryTaskConfig(ctx context.Context, task any) (map[string]any, error) {
	var cfg map[string]any
	if _, scoped := auxSessionAuxiliary(ctx); !scoped && auxTaskName(task) != "" {
		tree, err := auxLoadConfigReadonlyFn(ctx)
		if err != nil {
			return nil, err
		}
		cfg = tree
	}
	return newResolver(ctx, cfg, nil, "", nil).auxiliaryTaskConfig(task, map[string]bool{}), nil
}

// Merged auxiliary task config block for a task name
func AuxiliaryTaskConfig(ctx context.Context, task string) (map[string]any, error) {
	return auxGetAuxiliaryTaskConfig(ctx, task)
}

var auxProviderVisionModels = map[string]string{"xiaomi": "mimo-v2.5", "zai": "glm-5.3-flash"}

// Provider default vision model from the static table, else the profile's, empty for none
func auxResolveProviderVisionDefault(ctx context.Context, provider string) string {
	if static := auxProviderVisionModels[provider]; static != "" {
		return static
	}
	if profile, ok := GetProviderProfile(ctx, provider); ok {
		return profile.DefaultVisionModel(ctx)
	}
	return ""
}

var auxProvidersWithoutVision = map[string]bool{"kimi-coding": true, "kimi-coding-cn": true}
