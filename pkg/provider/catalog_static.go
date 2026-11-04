package provider

import (
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// CuratedFallbackModels marks a curated list served because the live catalog was unavailable
type CuratedFallbackModels []string

// CatalogModel is one picker row as model id plus the description shown in menus
type CatalogModel struct {
	ID          string
	Description string
}

var openRouterDescriptions = map[string]string{
	"anthropic/claude-opus-5-fast":      "2x price, higher output speed",
	"anthropic/claude-opus-4.8-fast":    "2x price, higher output speed",
	"deepseek/deepseek-v4-pro-0813":     "dated snapshot of v4-pro",
	"deepseek/deepseek-v4.1-flash-1017": "dated snapshot of v4.1-flash",
	"deepseek/deepseek-v4-flash-0731":   "dated snapshot of v4-flash",
	"moonshotai/kimi-k3":                "recommended",
	"z-ai/glm-5.3":                      "default",
	"z-ai/glm-5.3-flashx":               "high-speed tier of glm-5.3-flash",
	"openrouter/pareto-code":            "auto-routes to cheapest coder meeting openrouter.min_coding_score",
	"openai/gpt-6-astra-fast":           "2x price, priority tier",
	"openai/gpt-6-astra-flex":           "0.5x price, flex tier",
	"openai/gpt-6-astra-pro-fast":       "2x price, priority tier",
	"openai/gpt-6-astra-pro-flex":       "0.5x price, flex tier",
	"stealth/union-alpha":               "free, stealth model",
	"stealth/space-bunny-alpha":         "free, stealth model",
}

var openRouterModelIDs = []string{
	"anthropic/claude-fable-5.1", "anthropic/claude-fable-5", "anthropic/claude-opus-5.5",
	"anthropic/claude-opus-5", "anthropic/claude-opus-5-fast", "anthropic/claude-opus-4.8", "anthropic/claude-opus-4.8-fast",
	"anthropic/claude-sonnet-5.5", "anthropic/claude-sonnet-5", "anthropic/claude-haiku-5.5", "anthropic/claude-haiku-4.5",
	"openai/gpt-6-astra", "openai/gpt-6-astra-fast", "openai/gpt-6-astra-flex", "openai/gpt-6-astra-pro",
	"openai/gpt-6-astra-pro-fast", "openai/gpt-6-astra-pro-flex",
	"openai/gpt-6.1-sol", "openai/gpt-6.1-sol-pro", "openai/gpt-6.1-terra", "openai/gpt-6-sol", "openai/gpt-6-sol-pro",
	"openai/gpt-6-luna", "openai/gpt-6-luna-pro",
	"openai/gpt-5.5", "openai/gpt-5.5-pro", "openai/gpt-5.4-mini", "google/gemini-4-argon",
	"google/gemini-3.1-pro-preview",
	"google/gemini-3.8-flash", "google/gemini-3.7-flash", "x-ai/grok-4.7", "x-ai/grok-4.6",
	"deepseek/deepseek-v4-pro",
	"deepseek/deepseek-v4-pro-0813", "deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash-1017",
	"deepseek/deepseek-v4-flash-0731",
	"qwen/qwen3.8-max-0902", "qwen/qwen3.8-flash", "moonshotai/kimi-k3", "minimax/minimax-m3", "z-ai/glm-5.3",
	"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flashx", "z-ai/glm-5.2",
	"xiaomi/mimo-v2.6-pro", "xiaomi/mimo-v2.6-flash", "xiaomi/mimo-v2.6-pro-ultraspeed", "xiaomi/mimo-v2.5-pro", "tencent/hy4-preview",
	"tencent/hy3",
	"stepfun/step-3.7-flash", "nvidia/nemotron-3-super-120b-a12b", "meta/muse-spark-1.2",
	"meta/muse-spark-1.2-contributor", "meta/muse-spark-1.3", "meta/muse-spark-1.3-contributor", "sakana/fugu-ultra",
	"upstage/solar-pro4", "upstage/solar-mini4",
	"openrouter/pareto-code", "thinkingmachines/inkling:free", "thinkingmachines/inkling-small:free",
	"minimax/minimax-m3:free", "z-ai/glm-5.2:free", "poolside/laguna-s-2.1:free", "poolside/laguna-xs-2.1:free",
	"nvidia/nemotron-3-super-120b-a12b:free", "nvidia/nemotron-3-ultra-550b-a55b:free",
	"inclusionai/ling-3.0-flash:free", "nvidia/nemotron-3.5-lightning:free", "stealth/union-alpha",
	"stealth/space-bunny-alpha",
}

// OpenRouterModels is the fallback OpenRouter snapshot used when the live catalog is unavailable
var OpenRouterModels = func() []CatalogModel {
	out := make([]CatalogModel, 0, len(openRouterModelIDs))
	for _, mid := range openRouterModelIDs {
		desc, ok := openRouterDescriptions[mid]
		if !ok && strings.HasSuffix(mid, ":free") {
			desc = "free"
		}
		out = append(out, CatalogModel{ID: mid, Description: desc})
	}
	return out
}()

// OpenRouter entries the Nous Portal does not carry
var openRouterOnly = map[string]bool{
	"anthropic/claude-opus-5-fast": true, "anthropic/claude-opus-4.8-fast": true,
	"meta/muse-spark-1.2-contributor": true, "meta/muse-spark-1.3-contributor": true, "openrouter/pareto-code": true,
	"stealth/union-alpha": true,
}

// VercelAIGatewayModels is the fallback Vercel AI Gateway snapshot, open weight first then closed by family
var VercelAIGatewayModels = func() []CatalogModel {
	out := []CatalogModel{{ID: "moonshotai/kimi-k2.6", Description: "recommended"}}
	for _, mid := range []string{
		"alibaba/qwen3.6-plus", "zai/glm-5.1", "minimax/minimax-m2.7", "anthropic/claude-sonnet-4.6",
		"anthropic/claude-opus-4.7", "anthropic/claude-opus-4.6", "anthropic/claude-haiku-4.5",
		"openai/gpt-5.4", "openai/gpt-5.4-mini", "openai/gpt-5.3-codex", "google/gemini-3.1-pro-preview",
		"google/gemini-3-flash", "google/gemini-3.1-flash-lite-preview", "xai/grok-4.20-reasoning",
	} {
		out = append(out, CatalogModel{ID: mid})
	}
	return out
}()

// CatalogModelIDs returns the bare ids of a described snapshot
func CatalogModelIDs(rows []CatalogModel) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// Static xAI fallback when the models.dev disk cache is empty, retired 2026-05-15 ids excluded
var xaiStaticFallback = []string{
	"grok-4.6", "grok-build-0.1", "grok-4.5", "grok-4.3", "grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning", "grok-4.20-multi-agent-0309",
}

// Callable via xAI OAuth but omitted from models.dev and /v1/models listings
var xaiCuratedExtras = []string{"grok-4.6", "grok-4.5", "grok-composer-2.5-fast"}

const xaiTopModel = "grok-4.6"

// Pins the headline xAI model to the top of the curated list
func xaiPromoteTop(ids []string) []string {
	found := false
	for _, m := range ids {
		if m == xaiTopModel {
			found = true
			break
		}
	}
	if !found {
		return ids
	}
	out := []string{xaiTopModel}
	for _, m := range ids {
		if m != xaiTopModel {
			out = append(out, m)
		}
	}
	return out
}

// Appends curated xAI models missing from models.dev right after the pinned headline
func xaiMergeCuratedExtras(ids []string) []string {
	out := append([]string(nil), ids...)
	for _, extra := range xaiCuratedExtras {
		if containsString(out, extra) {
			continue
		}
		at := len(out)
		if len(out) > 0 && out[0] == xaiTopModel {
			at = 1
		}
		out = append(out[:at], append([]string{extra}, out[at:]...)...)
	}
	return out
}

func xaiFinalizeCatalog(ids []string) []string {
	return xaiPromoteTop(xaiMergeCuratedExtras(ids))
}

// Offline curated floor for xAI pickers from the models.dev disk cache, else the static list
func xaiCuratedModels() []string {
	data := fetchModelsDevIn(home.Process(), false)
	if xai, ok := asMap(data["xai"]); ok {
		if models, ok := asMap(xai["models"]); ok && len(models) > 0 {
			ids := make([]string, 0, len(models))
			for mid := range models {
				ids = append(ids, mid)
			}
			if len(ids) > 0 {
				sort.Strings(ids)
				return xaiFinalizeCatalog(ids)
			}
		}
	}
	return xaiFinalizeCatalog(append([]string(nil), xaiStaticFallback...))
}

var xaiRows = []string{"xai", "xai-oauth"}

// Codex curated list seam owned by the models slice
var codexCuratedModelsFn = codexCuratedModels

var openAIChatModels = []string{
	"gpt-5.4", "gpt-5.4-mini", "gpt-5-mini", "gpt-5.3-codex", "gpt-5.2-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
}
var minimaxModels = []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1", "MiniMax-M2"}
var tencentModels = []string{"hy4-preview", "hy3", "hy3-preview"}
var alibabaModels = []string{
	"qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "kimi-k2.5",
	"qwen3.5-plus", "qwen3-coder-plus", "qwen3-coder-next", "glm-5.2", "glm-5", "glm-4.7",
	"deepseek-v4-pro", "deepseek-v4-flash-0731", "MiniMax-M2.5",
}
var alibabaCodingPlanModels = []string{
	"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-plus",
	"qwen3-coder-next", "kimi-k2.5", "glm-5", "glm-4.7", "MiniMax-M2.5",
}
var alibabaTokenPlanModels = []string{
	"qwen3.8-max-0902", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro",
	"deepseek-v4-flash", "deepseek-v3.2", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "glm-5.2", "glm-5.1", "glm-5",
}

func cloneStrings(in []string) []string { return append([]string(nil), in...) }

// Nous rows are the OpenRouter snapshot minus OpenRouter only and free entries
func nousCuratedModels() []string {
	var out []string
	for _, row := range OpenRouterModels {
		if openRouterOnly[row.ID] || strings.HasSuffix(row.ID, ":free") {
			continue
		}
		out = append(out, row.ID)
	}
	return out
}

var (
	providerModelsMu   sync.Mutex
	providerModelsOnce sync.Once
	xaiReady           bool
	providerModels     map[string][]string
)

// Curated per provider lists with cn twins sharing the international catalog
func buildProviderModels() {
	providerModels = map[string][]string{
		"moa":    {"default"},
		"nous":   nousCuratedModels(),
		"openai": cloneStrings(openAIChatModels),
		"openai-api": {"gpt-6.1-sol", "gpt-6.1-sol-pro", "gpt-6-sol", "gpt-6-sol-pro", "gpt-6-luna", "gpt-6-luna-pro",
			"gpt-5.6-sol", "gpt-5.6-sol-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro", "gpt-5.6-luna",
			"gpt-5.6-luna-pro", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
			"gpt-5-mini", "gpt-5.3-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini"},
		"openai-codex": codexCuratedModelsFn(),
		"xai-oauth":    xaiFinalizeCatalog(cloneStrings(xaiStaticFallback)),
		"gemini": {"gemini-3.8-flash", "gemini-4-argon", "gemini-3.7-flash",
			"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3.6-flash", "gemini-3.1-flash-lite-preview"},
		"zai": {"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "glm-5v-turbo", "glm-5-turbo",
			"glm-4.7", "glm-4.5", "glm-4.5-flash"},
		"xai": xaiFinalizeCatalog(cloneStrings(xaiStaticFallback)),
		"nvidia": {"nvidia/nemotron-3.5-super-vl-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "nvidia/nemotron-3-super-120b-a12b",
			"nvidia/nemotron-3.5-lightning-30b-a3b", "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
			"z-ai/glm-5.3", "z-ai/glm-5.2", "moonshotai/kimi-k2.6", "minimaxai/minimax-m3"},
		"kimi-coding": {"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "kimi-for-coding", "kimi-for-coding-highspeed",
			"kimi-k2-thinking", "kimi-k2-thinking-turbo", "kimi-k2-turbo-preview", "kimi-k2-0905-preview"},
		"kimi-coding-cn": {"kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5",
			"kimi-k2-thinking", "kimi-k2-turbo-preview", "kimi-k2-0905-preview"},
		"stepfun":       {"step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603"},
		"moonshot":      {"kimi-k3", "kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2-turbo-preview", "kimi-k2-0905-preview"},
		"minimax":       cloneStrings(minimaxModels),
		"minimax-oauth": {"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed"},
		"minimax-cn":    cloneStrings(minimaxModels),
		"anthropic": {"claude-fable-5.1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5",
			"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
			"claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929",
			"claude-opus-4-20250514", "claude-sonnet-4-20250514", "claude-haiku-4-5-20251001"},
		"deepseek": {"deepseek-flash", "deepseek-v4-pro"},
		"xiaomi": {"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed",
			"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "mimo-v2-flash"},
		"tencent-tokenhub":  cloneStrings(tencentModels),
		"tencent-tokenplan": cloneStrings(tencentModels),
		"arcee":             {"trinity-large-thinking", "trinity-large-preview", "trinity-mini"},
		"gmi": {"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5",
			"google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5",
			"anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
		"opencode-zen": {"x-preview-f-free", "kimi-k3", "kimi-k2.5", "kimi-k2.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
			"gpt-5.5", "gpt-5.5-pro", "gpt-5.4-pro", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.3-codex",
			"gpt-5.3-codex-spark", "gpt-5.2", "gpt-5.2-codex", "gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-max",
			"gpt-5.1-codex-mini", "gpt-5", "gpt-5-codex", "gpt-5-nano", "claude-fable-5", "claude-opus-5",
			"claude-sonnet-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6", "claude-opus-4-5",
			"claude-sonnet-4-6", "claude-sonnet-4-5", "claude-sonnet-4", "claude-haiku-4-5", "gemini-3.8-flash",
			"gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.1-pro", "gemini-3-flash",
			"grok-4.6", "grok-4.5", "grok-build-0.1", "muse-spark-1.2", "minimax-m3", "minimax-m2.7", "minimax-m2.5",
			"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "kimi-k2.7-code", "deepseek-v4-pro",
			"deepseek-v4-flash", "qwen3.6-plus", "qwen3.5-plus", "big-pickle", "mimo-v2.5-free",
			"nemotron-3-ultra-free", "nemotron-3.5-lightning-free",
			"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free"},
		"opencode-go": {"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "gpt-5.6-luna", "grok-4.5", "glm-5.3",
			"glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro",
			"mimo-v2-omni", "minimax-m3", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro",
			"deepseek-v4-flash", "qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
			"qwen3.5-plus", "hy3", "hy3-preview", "muse-spark-1.2-contributor", "muse-spark-1.3-contributor"},
		"kilocode": {"anthropic/claude-opus-4.6", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4",
			"google/gemini-3-pro-preview", "google/gemini-3-flash-preview"},
		"alibaba":                cloneStrings(alibabaModels),
		"alibaba-cn":             cloneStrings(alibabaModels),
		"alibaba-coding-plan":    cloneStrings(alibabaCodingPlanModels),
		"alibaba-coding-plan-cn": cloneStrings(alibabaCodingPlanModels),
		"alibaba-token-plan":     cloneStrings(alibabaTokenPlanModels),
		"alibaba-token-plan-cn":  cloneStrings(alibabaTokenPlanModels),
		"huggingface": {"moonshotai/Kimi-K2.5", "Qwen/Qwen3.5-397B-A17B", "Qwen/Qwen3.5-35B-A3B",
			"deepseek-ai/DeepSeek-V3.2", "MiniMaxAI/MiniMax-M2.5", "zai-org/GLM-5",
			"XiaomiMiMo/MiMo-V2-Flash", "moonshotai/Kimi-K2-Thinking", "moonshotai/Kimi-K2.6"},
		"bedrock": {"us.anthropic.claude-sonnet-5", "us.anthropic.claude-opus-5-5", "us.anthropic.claude-sonnet-4-6",
			"us.anthropic.claude-opus-4-6-v1",
			"us.anthropic.claude-haiku-4-5-20251001-v1:0", "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
			"openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna",
			"openai.gpt-5.5", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.6-luna",
			"us.amazon.nova-pro-v1:0", "us.amazon.nova-lite-v1:0", "us.amazon.nova-micro-v1:0", "deepseek.v3.2",
			"us.meta.llama4-maverick-17b-instruct-v1:0", "us.meta.llama4-scout-17b-instruct-v1:0"},
		"azure-foundry": {},
		"vertex": {"google/gemini-3.8-flash", "google/gemini-4-argon", "google/gemini-3.7-flash",
			"google/gemini-3.1-pro-preview", "google/gemini-3-pro-preview", "google/gemini-3.6-flash",
			"google/gemini-3.5-flash", "google/gemini-3.5-flash-lite", "google/gemini-3-flash-preview",
			"google/gemini-3.1-flash-lite-preview", "google/gemini-3.1-flash-lite"},
		"novita": {"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324",
			"deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
		"ai-gateway": CatalogModelIDs(VercelAIGatewayModels),
	}
}

// Fills the xAI rows in place from the models.dev disk cache on first xAI read
func ensureXAIRows() {
	providerModelsMu.Lock()
	ready := xaiReady
	providerModelsMu.Unlock()
	if ready {
		return
	}
	ids := xaiCuratedModels()
	providerModelsMu.Lock()
	defer providerModelsMu.Unlock()
	if xaiReady {
		return
	}
	for _, row := range xaiRows {
		providerModels[row] = ids
	}
	xaiReady = true
}

func isXAIRow(provider string) bool { return containsString(xaiRows, provider) }

// Curated static model ids for one provider, empty when the provider has no row
func staticCatalogModels(provider string) []string {
	providerModelsOnce.Do(buildProviderModels)
	if isXAIRow(provider) {
		ensureXAIRows()
	}
	providerModelsMu.Lock()
	defer providerModelsMu.Unlock()
	return cloneStrings(providerModels[provider])
}

// HasCuratedCatalog reports whether the curated table has a row for the provider
func HasCuratedCatalog(provider string) bool {
	providerModelsOnce.Do(buildProviderModels)
	providerModelsMu.Lock()
	defer providerModelsMu.Unlock()
	_, ok := providerModels[provider]
	return ok
}

// CuratedCatalogRows returns a copy of every curated row with the xAI rows resolved
func CuratedCatalogRows() map[string][]string {
	providerModelsOnce.Do(buildProviderModels)
	ensureXAIRows()
	providerModelsMu.Lock()
	defer providerModelsMu.Unlock()
	out := make(map[string][]string, len(providerModels))
	for k, v := range providerModels {
		out[k] = cloneStrings(v)
	}
	return out
}

// CuratedCatalogProviders returns the curated row names in sorted order
func CuratedCatalogProviders() []string {
	providerModelsOnce.Do(buildProviderModels)
	providerModelsMu.Lock()
	defer providerModelsMu.Unlock()
	out := make([]string, 0, len(providerModels))
	for k := range providerModels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Hand written canonical rows, the single source of provider identity for every list surface
var builtinCanonicalProviders = []CanonicalProviderEntry{
	{"nous", "Nous Portal", "Nous Portal (Everything your agent needs, 300+ models with bundled tool use)"},
	{"fireworks", "Fireworks AI", "Fireworks AI (OpenAI-compatible direct model API)"},
	{"openrouter", "OpenRouter", "OpenRouter (Pay-per-use API aggregator)"},
	{"moa", "Mixture of Agents", "Mixture of Agents (named presets; aggregator acts after reference models)"},
	{"novita", "NovitaAI", "NovitaAI (Cloud: Model API, Agent Sandbox, GPU Cloud)"},
	{"lmstudio", "LM Studio", "LM Studio (Local desktop app with built-in model server)"},
	{"anthropic", "Anthropic", "Anthropic (Claude models via API key or Claude Code)"},
	{"openai-codex", "ChatGPT or Codex Subscription", "ChatGPT or Codex Subscription (Sign in with your ChatGPT account, uses Codex models)"},
	{"openai-api", "OpenAI API", "OpenAI API (api.openai.com, API key)"},
	{"alibaba", "Qwen Cloud", "Qwen Cloud / DashScope (Qwen + multi-provider)"},
	{"xai-oauth", "xAI Grok OAuth (SuperGrok / Premium+)", "xAI Grok OAuth (SuperGrok / Premium+ subscription)"},
	{"xiaomi", "Xiaomi MiMo", "Xiaomi MiMo (MiMo-V2.5 and V2 models: pro, omni, flash)"},
	{"tencent-tokenhub", "Tencent TokenHub", "Tencent TokenHub (Hy4 preview via tokenhub.tencentmaas.com)"},
	{"tencent-tokenplan", "Tencent TokenPlan", "Tencent TokenPlan (Hy4 preview via api.lkeap.cloud.tencent.com, Anthropic Messages)"},
	{"nvidia", "NVIDIA NIM", "NVIDIA NIM (Nemotron models via build.nvidia.com or local NIM)"},
	{"huggingface", "Hugging Face", "Hugging Face Inference Providers"},
	{"gemini", "Google AI Studio", "Google AI Studio (Native Gemini API)"},
	{"vertex", "Google Vertex AI", "Google Vertex AI (Gemini via GCP; OAuth2 service account or ADC, GCP billing/quotas)"},
	{"deepseek", "DeepSeek", "DeepSeek (V3, R1, coder, direct API)"},
	{"xai", "xAI", "xAI Grok (Direct API)"},
	{"zai", "Z.AI / GLM", "Z.AI / GLM (Zhipu direct API)"},
	{"kimi-coding", "Kimi / Kimi Coding Plan", "Kimi Coding Plan (api.kimi.com & Moonshot API)"},
	{"kimi-coding-cn", "Kimi / Moonshot (China)", "Kimi / Moonshot China (Domestic direct API)"},
	{"stepfun", "StepFun Step Plan", "StepFun Step Plan (Agent / coding models via Step Plan API)"},
	{"minimax", "MiniMax", "MiniMax (Global direct API)"},
	{"minimax-oauth", "MiniMax (OAuth)", "MiniMax via OAuth browser login (Coding Plan, minimax.io)"},
	{"minimax-cn", "MiniMax (China)", "MiniMax China (Domestic direct API)"},
	{"ollama-cloud", "Ollama Cloud", "Ollama Cloud (Cloud-hosted open models, ollama.com)"},
	{"arcee", "Arcee AI", "Arcee AI (Trinity models, direct API)"},
	{"gmi", "GMI Cloud", "GMI Cloud (Multi-model direct API)"},
	{"kilocode", "Kilo Code", "Kilo Code (Kilo Gateway API)"},
	{"opencode-zen", "OpenCode Zen", "OpenCode Zen (Curated models, pay-as-you-go)"},
	{"opencode-go", "OpenCode Go", "OpenCode Go (Open models subscription)"},
	{"bedrock", "AWS Bedrock", "AWS Bedrock (Claude, Nova, Llama, DeepSeek; IAM or API key)"},
	{"azure-foundry", "Azure Foundry", "Azure Foundry (OpenAI-style or Anthropic-style endpoint, your Azure AI deployment)"},
	{"ai-gateway", "Vercel AI Gateway", "Vercel AI Gateway (Multi-model aggregator)"},
	{"qwen-oauth", "Qwen OAuth (Portal)", "Qwen OAuth (Reuses local Qwen CLI login)"},
}

// ProviderGroup folds several slugs of one vendor under a single interactive picker row
type ProviderGroup struct {
	ID          string
	Label       string
	Description string
	Members     []string
}

// PickerRow is one folded picker row, a single slug or a group of member slugs
type PickerRow struct {
	Kind        string
	Slug        string
	GroupID     string
	Label       string
	Description string
	Members     []string
}

// PluginPickerPlacement is the catalog placement a plugin profile declares beyond its profile row
type PluginPickerPlacement struct {
	After          string
	Group          ProviderGroup
	CatalogAliases bool
}

var (
	catalogMu            sync.Mutex
	catalogOnce          sync.Once
	canonicalProviders   []CanonicalProviderEntry
	canonicalSlugs       map[string]bool
	builtinSlugs         map[string]bool
	providerLabels       map[string]string
	providerGroups       []ProviderGroup
	slugToGroup          map[string]string
	aggregatorProviders  map[string]bool
	pluginPlacements     = map[string]PluginPickerPlacement{}
	pluginCatalogAliases = map[string]string{}
	catalogBuilt         bool
)

func groupIndex(id string) int {
	for i, g := range providerGroups {
		if g.ID == id {
			return i
		}
	}
	return -1
}

// Seeds the canonical rows, labels, groups and aggregator set then admits plugin profiles
func buildCatalogTables() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	canonicalProviders = append([]CanonicalProviderEntry(nil), builtinCanonicalProviders...)
	canonicalSlugs = map[string]bool{}
	builtinSlugs = map[string]bool{}
	providerLabels = map[string]string{}
	for _, p := range canonicalProviders {
		canonicalSlugs[p.Slug] = true
		builtinSlugs[p.Slug] = true
		providerLabels[p.Slug] = p.Label
	}
	providerLabels["custom"] = "Custom endpoint"
	providerGroups = []ProviderGroup{
		{"kimi", "Kimi / Moonshot", "Coding Plan, Moonshot global & China endpoints", []string{"kimi-coding", "kimi-coding-cn"}},
		{"minimax", "MiniMax", "Global, OAuth Coding Plan & China endpoints", []string{"minimax", "minimax-oauth", "minimax-cn"}},
		{"xai", "xAI Grok", "Direct API or SuperGrok / Premium+ OAuth", []string{"xai", "xai-oauth"}},
		{"google", "Google Gemini", "Google AI Studio (API key)", []string{"gemini"}},
		{"openai", "OpenAI", "ChatGPT/Codex subscription or direct OpenAI API", []string{"openai-codex", "openai-api"}},
		{"qwen", "Qwen", "Qwen Cloud / DashScope, Coding Plan, Token Plan & Qwen CLI OAuth", []string{"alibaba", "alibaba-cn", "alibaba-coding-plan", "alibaba-coding-plan-cn", "alibaba-token-plan", "alibaba-token-plan-cn", "qwen-oauth"}},
		{"opencode", "OpenCode", "Zen pay-as-you-go or Go subscription", []string{"opencode-zen", "opencode-go"}},
		{"tencent", "Tencent Hy", "Hy4 / Hy3 via TokenHub & TokenPlan", []string{"tencent-tokenhub", "tencent-tokenplan"}},
	}
	slugToGroup = map[string]string{}
	for _, g := range providerGroups {
		for _, m := range g.Members {
			slugToGroup[m] = g.ID
		}
	}
	aggregatorProviders = map[string]bool{"nous": true, "openrouter": true, "ai-gateway": true, "kilocode": true}
	syncPluginProviderCatalogLocked()
	catalogBuilt = true
}

// RegisterPluginPickerPlacement records where an out of tree plugin profile lands in the picker tables
func RegisterPluginPickerPlacement(name string, placement PluginPickerPlacement) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	pluginPlacements[strings.ToLower(strings.TrimSpace(name))] = placement
	if catalogBuilt {
		syncPluginProviderCatalogLocked()
	}
}

// Picker admission for a plugin profile, any slug without a built in row, catalog lock held
func pluginProviderEntersPicker(name string) bool {
	return !canonicalSlugs[name]
}

// PluginProviderEntersPicker reports whether a profile slug still lacks a row in the canonical table
func PluginProviderEntersPicker(name string) bool {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return pluginProviderEntersPicker(name)
}

// SyncPluginProviderCatalog admits every registered profile without a built in row and returns how many were added
func SyncPluginProviderCatalog() int {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return syncPluginProviderCatalogLocked()
}

// Admits pending plugin rows with picker_after chains resolved in declared order
func syncPluginProviderCatalogLocked() int {
	var pending []Profile
	for _, pp := range Profiles() {
		if pluginProviderEntersPicker(pp.Name) {
			pending = append(pending, pp)
		}
	}
	added := 0
	for len(pending) > 0 {
		names := map[string]bool{}
		for _, pp := range pending {
			names[pp.Name] = true
		}
		var ready []Profile
		for _, pp := range pending {
			if !names[pluginPlacements[pp.Name].After] {
				ready = append(ready, pp)
			}
		}
		if len(ready) == 0 {
			ready = pending
		}
		readySet := map[string]bool{}
		for _, pp := range ready {
			admitPluginProvider(pp)
			readySet[pp.Name] = true
			added++
		}
		var rest []Profile
		for _, pp := range pending {
			if !readySet[pp.Name] {
				rest = append(rest, pp)
			}
		}
		pending = rest
	}
	return added
}

// Adds one plugin profile to the row table at its anchor plus label, group and aggregator set
func admitPluginProvider(pp Profile) {
	label := pp.Label()
	desc := pp.Description
	if desc == "" {
		desc = label + " (direct API)"
	}
	entry := CanonicalProviderEntry{Slug: pp.Name, Label: label, TUIDesc: desc}
	placement := pluginPlacements[pp.Name]
	at := len(canonicalProviders)
	for i, p := range canonicalProviders {
		if p.Slug == placement.After {
			at = i + 1
			break
		}
	}
	canonicalProviders = append(canonicalProviders[:at], append([]CanonicalProviderEntry{entry}, canonicalProviders[at:]...)...)
	canonicalSlugs[pp.Name] = true
	providerLabels[pp.Name] = label
	if placement.Group.ID != "" {
		gi := groupIndex(placement.Group.ID)
		if gi < 0 {
			providerGroups = append(providerGroups, ProviderGroup{ID: placement.Group.ID, Label: placement.Group.Label, Description: placement.Group.Description})
			gi = len(providerGroups) - 1
		}
		if !containsString(providerGroups[gi].Members, pp.Name) {
			providerGroups[gi].Members = append(providerGroups[gi].Members, pp.Name)
		}
		slugToGroup[pp.Name] = placement.Group.ID
	}
	if pp.Aggregator {
		aggregatorProviders[pp.Name] = true
	}
	if placement.CatalogAliases && !builtinSlugs[pp.Name] {
		for _, alias := range append(cloneStrings(pp.Aliases), staticAliasGroups[pp.Name]...) {
			if _, ok := catalogProviderAliases[alias]; ok {
				continue
			}
			if _, ok := pluginCatalogAliases[alias]; !ok {
				pluginCatalogAliases[alias] = pp.Name
			}
		}
	}
}

// CanonicalProviders returns the full row table including hidden plugin profiles
func CanonicalProviders() []CanonicalProviderEntry {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return append([]CanonicalProviderEntry(nil), canonicalProviders...)
}

// IsCanonicalProvider reports whether the slug has a row in the canonical table
func IsCanonicalProvider(slug string) bool {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return canonicalSlugs[slug]
}

// Hidden profiles the user has not opted into by adding a credential pool row for them
func unlistedProviderNames() map[string]bool {
	hidden := map[string]bool{}
	store := NewStore(home.Process())
	for _, p := range Profiles() {
		if !p.Hidden {
			continue
		}
		rows, err := store.ReadCredentialPool(p.Name)
		if err != nil || len(rows) == 0 {
			hidden[p.Name] = true
		}
	}
	return hidden
}

// Canonical rows minus pre release profiles the user has not opted into
func listedCanonicalProviders() []CanonicalProviderEntry {
	hidden := unlistedProviderNames()
	var out []CanonicalProviderEntry
	for _, p := range CanonicalProviders() {
		if !hidden[p.Slug] {
			out = append(out, p)
		}
	}
	return out
}

// ListedCanonicalProviders is the exported form of the picker row list
func ListedCanonicalProviders() []CanonicalProviderEntry { return listedCanonicalProviders() }

// CatalogProviderLabel returns the short display label for a slug, empty when unknown
func CatalogProviderLabel(slug string) string {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return providerLabels[slug]
}

// ProviderGroups returns the display groups in declaration order
func ProviderGroups() []ProviderGroup {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	out := make([]ProviderGroup, len(providerGroups))
	for i, g := range providerGroups {
		out[i] = g
		out[i].Members = cloneStrings(g.Members)
	}
	return out
}

// ProviderGroupForSlug returns the group id a slug belongs to, empty when ungrouped
func ProviderGroupForSlug(slug string) string {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return slugToGroup[strings.ToLower(strings.TrimSpace(slug))]
}

// GroupProviders folds an ordered slug list into picker rows, a group at its first member's position
func GroupProviders(slugs []string) []PickerRow {
	groups := ProviderGroups()
	present := map[string]bool{}
	for _, s := range slugs {
		present[s] = true
	}
	groupMembers := map[string][]string{}
	byID := map[string]ProviderGroup{}
	for _, g := range groups {
		byID[g.ID] = g
		var members []string
		for _, m := range g.Members {
			if present[m] {
				members = append(members, m)
			}
		}
		groupMembers[g.ID] = members
	}
	var rows []PickerRow
	seen := map[string]bool{}
	emitted := map[string]bool{}
	for _, slug := range slugs {
		s := strings.ToLower(strings.TrimSpace(slug))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		gid := ProviderGroupForSlug(s)
		if gid == "" {
			rows = append(rows, PickerRow{Kind: "single", Slug: s})
			continue
		}
		if emitted[gid] {
			continue
		}
		emitted[gid] = true
		members := groupMembers[gid]
		if len(members) == 0 {
			members = []string{s}
		}
		if len(members) <= 1 {
			rows = append(rows, PickerRow{Kind: "single", Slug: members[0]})
			continue
		}
		g := byID[gid]
		rows = append(rows, PickerRow{Kind: "group", GroupID: gid, Label: g.Label, Description: g.Description, Members: cloneStrings(members)})
	}
	return rows
}

// IsAggregatorProvider reports membership in the catalog aggregator set including plugin admissions
func IsAggregatorProvider(slug string) bool {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	return aggregatorProviders[slug]
}

// AggregatorProviders returns the catalog aggregator set in sorted order
func AggregatorProviders() []string {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	out := make([]string, 0, len(aggregatorProviders))
	for k := range aggregatorProviders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Built in catalog alias table for the model picker and typed provider names
var catalogProviderAliases = map[string]string{
	"glm": "zai", "z-ai": "zai", "z.ai": "zai", "zhipu": "zai", "google": "gemini",
	"google-gemini": "gemini", "google-ai-studio": "gemini", "google-vertex": "vertex", "vertex-ai": "vertex",
	"gcp-vertex": "vertex", "vertexai": "vertex", "kimi": "kimi-coding", "moonshot": "kimi-coding",
	"kimi-cn": "kimi-coding-cn", "moonshot-cn": "kimi-coding-cn", "step": "stepfun",
	"stepfun-coding-plan": "stepfun", "arcee-ai": "arcee", "arceeai": "arcee", "gmi-cloud": "gmi",
	"gmicloud": "gmi", "fireworks-ai": "fireworks", "fw": "fireworks", "actual-computer": "actual",
	"actualcomputer": "actual", "aci": "actual", "nebius": "nebius-token-factory",
	"nebius-tokenfactory": "nebius-token-factory", "nebius-tf": "nebius-token-factory",
	"token-factory": "nebius-token-factory", "tokenfactory": "nebius-token-factory",
	"minimax-china": "minimax-cn", "minimax_cn": "minimax-cn", "minimax-portal": "minimax-oauth",
	"minimax-global": "minimax-oauth", "minimax_oauth": "minimax-oauth", "claude": "anthropic",
	"claude-code": "anthropic", "deep-seek": "deepseek", "opencode": "opencode-zen", "zen": "opencode-zen",
	"go": "opencode-go", "opencode-go-sub": "opencode-go", "aigateway": "ai-gateway", "vercel": "ai-gateway",
	"vercel-ai-gateway": "ai-gateway", "kilo": "kilocode", "kilo-code": "kilocode",
	"kilo-gateway": "kilocode", "dashscope": "alibaba", "aliyun": "alibaba", "qwen": "alibaba",
	"alibaba-cloud": "alibaba", "qwen-portal": "qwen-oauth", "hf": "huggingface",
	"hugging-face": "huggingface", "huggingface-hub": "huggingface", "novita-ai": "novita",
	"novitaai": "novita", "mimo": "xiaomi", "xiaomi-mimo": "xiaomi", "tencent": "tencent-tokenhub",
	"tokenhub": "tencent-tokenhub", "tencent-cloud": "tencent-tokenhub", "tencentmaas": "tencent-tokenhub",
	"tokenplan": "tencent-tokenplan", "tencent-lkeap": "tencent-tokenplan", "aws": "bedrock",
	"aws-bedrock": "bedrock", "amazon-bedrock": "bedrock", "amazon": "bedrock", "grok": "xai",
	"grok-oauth": "xai-oauth", "xai-oauth": "xai-oauth", "x-ai-oauth": "xai-oauth",
	"xai-grok-oauth": "xai-oauth", "x-ai": "xai", "x.ai": "xai", "nim": "nvidia", "nvidia-nim": "nvidia",
	"build-nvidia": "nvidia", "nemotron": "nvidia", "lmstudio": "lmstudio", "lm-studio": "lmstudio",
	"lm_studio": "lmstudio", "chatgpt": "openai-codex", "chatgpt-codex": "openai-codex",
	"ollama":       "custom",
	"ollama_cloud": "ollama-cloud",
	"local":        "custom", "vllm": "custom",
}

// CatalogProviderAlias resolves a picker alias to its provider with built in aliases beating plugin ones
func CatalogProviderAlias(name string) (string, bool) {
	if target, ok := catalogProviderAliases[name]; ok {
		return target, true
	}
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	target, ok := pluginCatalogAliases[name]
	return target, ok
}

// CatalogProviderAliases returns every alias pair, built in first then plugin declared
func CatalogProviderAliases() map[string]string {
	catalogOnce.Do(buildCatalogTables)
	catalogMu.Lock()
	defer catalogMu.Unlock()
	out := make(map[string]string, len(catalogProviderAliases)+len(pluginCatalogAliases))
	for k, v := range pluginCatalogAliases {
		out[k] = v
	}
	for k, v := range catalogProviderAliases {
		out[k] = v
	}
	return out
}

// PreferredSilentDefaultModel is the offline fallback for the model landed on when the user never picked one
const PreferredSilentDefaultModel = "z-ai/glm-5.3"

// Providers whose silent auto default goes through the cost safe catalog labeled default
var silentDefaultProviders = map[string]bool{"nous": true, "openrouter": true}

// UsesSilentCatalogDefault reports whether a provider's silent default comes from the catalog label
func UsesSilentCatalogDefault(provider string) bool { return silentDefaultProviders[provider] }

// Retired model ids kept for /model auto detect only
var providerRetiredAliases = map[string][]string{
	"deepseek": {"deepseek-chat", "deepseek-reasoner"},
}

// ProviderRetiredAliases returns the retired ids a provider still auto detects
func ProviderRetiredAliases(provider string) []string {
	return cloneStrings(providerRetiredAliases[provider])
}

// Subscription providers whose catalogs re expose other vendors' models, tried last for bare aliases
var borrowedModelProviders = map[string]bool{}

// IsBorrowedModelProvider reports whether a provider only re exposes other vendors' models
func IsBorrowedModelProvider(provider string) bool { return borrowedModelProviders[provider] }

// Providers whose live /v1/models leads the picker with curated only entries appended
var liveFirstPickerProviders = map[string]bool{"opencode-zen": true, "opencode-go": true, "meta-ai": true}

// IsLiveFirstPickerProvider reports whether live entries lead the picker for the provider
func IsLiveFirstPickerProvider(provider string) bool { return liveFirstPickerProviders[provider] }

// Providers where models.dev is authoritative and the curated list is only an offline floor
var modelsDevPreferred = map[string]bool{
	"opencode-go": true, "opencode-zen": true, "kilocode": true, "fireworks": true, "mistral": true, "togetherai": true, "cohere": true,
	"perplexity": true, "groq": true, "nvidia": true, "huggingface": true, "zai": true, "gemini": true, "google": true, "xai": true, "xai-oauth": true,
}

// IsModelsDevPreferred reports whether models.dev beats the curated list for the provider
func IsModelsDevPreferred(provider string) bool { return modelsDevPreferred[provider] }
