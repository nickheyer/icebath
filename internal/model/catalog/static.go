package catalog

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/model/providers"
)

type ModelChoice struct {
	ID          string
	Description string
}

type ProviderGroup struct {
	ID          string
	Label       string
	Description string
	Members     []string
}

type PickerRow struct {
	Group       bool
	Slug        string
	GroupID     string
	Label       string
	Description string
	Members     []string
}

type ModelIdentity struct {
	Vendor string
	Family string
}

type staticRow struct {
	provider string
	models   []string
}

type aliasRow struct {
	alias    string
	identity ModelIdentity
}

const (
	PreferredSilentDefaultModel = "z-ai/glm-5.3"
	xaiTopModel                 = "grok-4.6"
	customProviderLabel         = "Custom endpoint"
)

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

var openRouterSnapshotIDs = []string{
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

var openRouterOnly = []string{
	"anthropic/claude-opus-5-fast", "anthropic/claude-opus-4.8-fast",
	"meta/muse-spark-1.2-contributor", "meta/muse-spark-1.3-contributor", "openrouter/pareto-code",
	"stealth/union-alpha",
}

var openRouterSnapshot = func() []ModelChoice {
	out := make([]ModelChoice, 0, len(openRouterSnapshotIDs))
	for _, id := range openRouterSnapshotIDs {
		desc, ok := openRouterDescriptions[id]
		if !ok && strings.HasSuffix(id, ":free") {
			desc = "free"
		}
		out = append(out, ModelChoice{ID: id, Description: desc})
	}
	return out
}()

var aiGatewaySnapshot = func() []ModelChoice {
	out := []ModelChoice{{ID: "moonshotai/kimi-k2.6", Description: "recommended"}}
	for _, id := range []string{
		"alibaba/qwen3.6-plus", "zai/glm-5.1", "minimax/minimax-m2.7", "anthropic/claude-sonnet-4.6",
		"anthropic/claude-opus-4.7", "anthropic/claude-opus-4.6", "anthropic/claude-haiku-4.5",
		"openai/gpt-5.4", "openai/gpt-5.4-mini", "openai/gpt-5.3-codex", "google/gemini-3.1-pro-preview",
		"google/gemini-3-flash", "google/gemini-3.1-flash-lite-preview", "xai/grok-4.20-reasoning",
	} {
		out = append(out, ModelChoice{ID: id})
	}
	return out
}()

var xaiStaticFallback = []string{
	"grok-4.6", "grok-build-0.1", "grok-4.5", "grok-4.3", "grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning", "grok-4.20-multi-agent-0309",
}

var xaiCuratedExtras = []string{"grok-4.6", "grok-4.5", "grok-composer-2.5-fast"}

var xaiRows = []string{"xai", "xai-oauth"}

var (
	openAIChatModels = []string{
		"gpt-5.4", "gpt-5.4-mini", "gpt-5-mini", "gpt-5.3-codex", "gpt-5.2-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
	}
	miniMaxModels = []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.5", "MiniMax-M2.1", "MiniMax-M2"}
	tencentModels = []string{"hy4-preview", "hy3", "hy3-preview"}
	alibabaModels = []string{
		"qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "kimi-k2.5",
		"qwen3.5-plus", "qwen3-coder-plus", "qwen3-coder-next", "glm-5.2", "glm-5", "glm-4.7",
		"deepseek-v4-pro", "deepseek-v4-flash-0731", "MiniMax-M2.5",
	}
	alibabaCodingPlanModels = []string{
		"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-plus",
		"qwen3-coder-next", "kimi-k2.5", "glm-5", "glm-4.7", "MiniMax-M2.5",
	}
	alibabaTokenPlanModels = []string{
		"qwen3.8-max-0902", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.6-flash", "deepseek-v4-pro",
		"deepseek-v4-flash", "deepseek-v3.2", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "glm-5.2", "glm-5.1", "glm-5",
	}
)

func choiceIDs(choices []ModelChoice) []string {
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		out = append(out, choice.ID)
	}
	return out
}

func portalSnapshotModels() []string {
	out := []string{}
	for _, choice := range openRouterSnapshot {
		if slices.Contains(openRouterOnly, choice.ID) || strings.HasSuffix(choice.ID, ":free") {
			continue
		}
		out = append(out, choice.ID)
	}
	return out
}

func xaiPromoteTop(ids []string) []string {
	if !slices.Contains(ids, xaiTopModel) {
		return ids
	}
	out := []string{xaiTopModel}
	for _, id := range ids {
		if id != xaiTopModel {
			out = append(out, id)
		}
	}
	return out
}

func xaiMergeCuratedExtras(ids []string) []string {
	out := slices.Clone(ids)
	for _, extra := range xaiCuratedExtras {
		if slices.Contains(out, extra) {
			continue
		}
		at := len(out)
		if len(out) > 0 && out[0] == xaiTopModel {
			at = 1
		}
		out = slices.Insert(out, at, extra)
	}
	return out
}

func xaiFinalizeCatalog(ids []string) []string {
	return xaiPromoteTop(xaiMergeCuratedExtras(ids))
}

var staticRows = []staticRow{
	{"moa", []string{"default"}},
	{providers.ProviderPortal, portalSnapshotModels()},
	{"openai", openAIChatModels},
	{"openai-api", []string{
		"gpt-6.1-sol", "gpt-6.1-sol-pro", "gpt-6-sol", "gpt-6-sol-pro", "gpt-6-luna", "gpt-6-luna-pro",
		"gpt-5.6-sol", "gpt-5.6-sol-pro", "gpt-5.6-terra", "gpt-5.6-terra-pro", "gpt-5.6-luna",
		"gpt-5.6-luna-pro", "gpt-5.5", "gpt-5.5-pro", "gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano",
		"gpt-5-mini", "gpt-5.3-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
	}},
	{"openai-codex", finalizeCodexModels(defaultCodexModels)},
	{"xai-oauth", xaiFinalizeCatalog(xaiStaticFallback)},
	{"gemini", []string{
		"gemini-3.8-flash", "gemini-4-argon", "gemini-3.7-flash",
		"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3.6-flash", "gemini-3.1-flash-lite-preview",
	}},
	{"zai", []string{
		"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "glm-5v-turbo", "glm-5-turbo",
		"glm-4.7", "glm-4.5", "glm-4.5-flash",
	}},
	{"xai", xaiFinalizeCatalog(xaiStaticFallback)},
	{"nvidia", []string{
		"nvidia/nemotron-3.5-super-vl-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "nvidia/nemotron-3-super-120b-a12b",
		"nvidia/nemotron-3.5-lightning-30b-a3b", "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning",
		"z-ai/glm-5.3", "z-ai/glm-5.2", "moonshotai/kimi-k2.6", "minimaxai/minimax-m3",
	}},
	{"kimi-coding", []string{
		"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "kimi-for-coding", "kimi-for-coding-highspeed",
		"kimi-k2-thinking", "kimi-k2-thinking-turbo", "kimi-k2-turbo-preview", "kimi-k2-0905-preview",
	}},
	{"kimi-coding-cn", []string{
		"kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5",
		"kimi-k2-thinking", "kimi-k2-turbo-preview", "kimi-k2-0905-preview",
	}},
	{"stepfun", []string{"step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603"}},
	{"moonshot", []string{
		"kimi-k3", "kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2-turbo-preview", "kimi-k2-0905-preview",
	}},
	{"minimax", miniMaxModels},
	{"minimax-oauth", []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed"}},
	{"minimax-cn", miniMaxModels},
	{"anthropic", []string{
		"claude-fable-5.1", "claude-fable-5", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5",
		"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929",
		"claude-opus-4-20250514", "claude-sonnet-4-20250514", "claude-haiku-4-5-20251001",
	}},
	{"deepseek", []string{"deepseek-flash", "deepseek-v4-pro"}},
	{"xiaomi", []string{
		"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.6-pro-ultraspeed",
		"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro", "mimo-v2-omni", "mimo-v2-flash",
	}},
	{"tencent-tokenhub", tencentModels},
	{"tencent-tokenplan", tencentModels},
	{"arcee", []string{"trinity-large-thinking", "trinity-large-preview", "trinity-mini"}},
	{"gmi", []string{
		"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5",
		"google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5",
		"anthropic/claude-sonnet-4.6", "openai/gpt-5.4",
	}},
	{"opencode-zen", []string{
		"x-preview-f-free", "kimi-k3", "kimi-k2.5", "kimi-k2.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
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
		"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free",
	}},
	{"opencode-go", []string{
		"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "gpt-5.6-luna", "grok-4.5", "glm-5.3",
		"glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro",
		"mimo-v2-omni", "minimax-m3", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro",
		"deepseek-v4-flash", "qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
		"qwen3.5-plus", "hy3", "hy3-preview", "muse-spark-1.2-contributor", "muse-spark-1.3-contributor",
	}},
	{"kilocode", []string{
		"anthropic/claude-opus-4.6", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4",
		"google/gemini-3-pro-preview", "google/gemini-3-flash-preview",
	}},
	{"alibaba", alibabaModels},
	{"alibaba-cn", alibabaModels},
	{"alibaba-coding-plan", alibabaCodingPlanModels},
	{"alibaba-coding-plan-cn", alibabaCodingPlanModels},
	{"alibaba-token-plan", alibabaTokenPlanModels},
	{"alibaba-token-plan-cn", alibabaTokenPlanModels},
	{"huggingface", []string{
		"moonshotai/Kimi-K2.5", "Qwen/Qwen3.5-397B-A17B", "Qwen/Qwen3.5-35B-A3B",
		"deepseek-ai/DeepSeek-V3.2", "MiniMaxAI/MiniMax-M2.5", "zai-org/GLM-5",
		"XiaomiMiMo/MiMo-V2-Flash", "moonshotai/Kimi-K2-Thinking", "moonshotai/Kimi-K2.6",
	}},
	{"bedrock", []string{
		"us.anthropic.claude-sonnet-5", "us.anthropic.claude-opus-5-5", "us.anthropic.claude-sonnet-4-6",
		"us.anthropic.claude-opus-4-6-v1",
		"us.anthropic.claude-haiku-4-5-20251001-v1:0", "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna",
		"openai.gpt-5.5", "openai.gpt-5.6-sol", "openai.gpt-5.6-terra", "openai.gpt-5.6-luna",
		"us.amazon.nova-pro-v1:0", "us.amazon.nova-lite-v1:0", "us.amazon.nova-micro-v1:0", "deepseek.v3.2",
		"us.meta.llama4-maverick-17b-instruct-v1:0", "us.meta.llama4-scout-17b-instruct-v1:0",
	}},
	{"azure-foundry", []string{}},
	{"vertex", []string{
		"google/gemini-3.8-flash", "google/gemini-4-argon", "google/gemini-3.7-flash",
		"google/gemini-3.1-pro-preview", "google/gemini-3-pro-preview", "google/gemini-3.6-flash",
		"google/gemini-3.5-flash", "google/gemini-3.5-flash-lite", "google/gemini-3-flash-preview",
		"google/gemini-3.1-flash-lite-preview", "google/gemini-3.1-flash-lite",
	}},
	{"novita", []string{
		"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324",
		"deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8",
	}},
	{"ai-gateway", choiceIDs(aiGatewaySnapshot)},
}

var staticRowIndex = func() map[string]int {
	out := make(map[string]int, len(staticRows))
	for i, row := range staticRows {
		out[row.provider] = i
	}
	return out
}()

var builtinCanonical = []providers.CanonicalEntry{
	{Slug: providers.ProviderPortal, Label: "Icebath Portal", TUIDesc: "Icebath Portal (Everything your agent needs, 300+ models with bundled tool use)"},
	{Slug: "fireworks", Label: "Fireworks AI", TUIDesc: "Fireworks AI (OpenAI-compatible direct model API)"},
	{Slug: "openrouter", Label: "OpenRouter", TUIDesc: "OpenRouter (Pay-per-use API aggregator)"},
	{Slug: "moa", Label: "Mixture of Agents", TUIDesc: "Mixture of Agents (named presets; aggregator acts after reference models)"},
	{Slug: "novita", Label: "NovitaAI", TUIDesc: "NovitaAI (Cloud: Model API, Agent Sandbox, GPU Cloud)"},
	{Slug: "lmstudio", Label: "LM Studio", TUIDesc: "LM Studio (Local desktop app with built-in model server)"},
	{Slug: "anthropic", Label: "Anthropic", TUIDesc: "Anthropic (Claude models via API key or Claude Code)"},
	{Slug: "openai-codex", Label: "ChatGPT or Codex Subscription", TUIDesc: "ChatGPT or Codex Subscription (Sign in with your ChatGPT account, uses Codex models)"},
	{Slug: "openai-api", Label: "OpenAI API", TUIDesc: "OpenAI API (api.openai.com, API key)"},
	{Slug: "alibaba", Label: "Qwen Cloud", TUIDesc: "Qwen Cloud / DashScope (Qwen + multi-provider)"},
	{Slug: "xai-oauth", Label: "xAI Grok OAuth (SuperGrok / Premium+)", TUIDesc: "xAI Grok OAuth (SuperGrok / Premium+ subscription)"},
	{Slug: "xiaomi", Label: "Xiaomi MiMo", TUIDesc: "Xiaomi MiMo (MiMo-V2.5 and V2 models: pro, omni, flash)"},
	{Slug: "tencent-tokenhub", Label: "Tencent TokenHub", TUIDesc: "Tencent TokenHub (Hy4 preview via tokenhub.tencentmaas.com)"},
	{Slug: "tencent-tokenplan", Label: "Tencent TokenPlan", TUIDesc: "Tencent TokenPlan (Hy4 preview via api.lkeap.cloud.tencent.com, Anthropic Messages)"},
	{Slug: "nvidia", Label: "NVIDIA NIM", TUIDesc: "NVIDIA NIM (Nemotron models via build.nvidia.com or local NIM)"},
	{Slug: "huggingface", Label: "Hugging Face", TUIDesc: "Hugging Face Inference Providers"},
	{Slug: "gemini", Label: "Google AI Studio", TUIDesc: "Google AI Studio (Native Gemini API)"},
	{Slug: "vertex", Label: "Google Vertex AI", TUIDesc: "Google Vertex AI (Gemini via GCP; OAuth2 service account or ADC, GCP billing/quotas)"},
	{Slug: "deepseek", Label: "DeepSeek", TUIDesc: "DeepSeek (V3, R1, coder, direct API)"},
	{Slug: "xai", Label: "xAI", TUIDesc: "xAI Grok (Direct API)"},
	{Slug: "zai", Label: "Z.AI / GLM", TUIDesc: "Z.AI / GLM (Zhipu direct API)"},
	{Slug: "kimi-coding", Label: "Kimi / Kimi Coding Plan", TUIDesc: "Kimi Coding Plan (api.kimi.com & Moonshot API)"},
	{Slug: "kimi-coding-cn", Label: "Kimi / Moonshot (China)", TUIDesc: "Kimi / Moonshot China (Domestic direct API)"},
	{Slug: "stepfun", Label: "StepFun Step Plan", TUIDesc: "StepFun Step Plan (Agent / coding models via Step Plan API)"},
	{Slug: "minimax", Label: "MiniMax", TUIDesc: "MiniMax (Global direct API)"},
	{Slug: "minimax-oauth", Label: "MiniMax (OAuth)", TUIDesc: "MiniMax via OAuth browser login (Coding Plan, minimax.io)"},
	{Slug: "minimax-cn", Label: "MiniMax (China)", TUIDesc: "MiniMax China (Domestic direct API)"},
	{Slug: "ollama-cloud", Label: "Ollama Cloud", TUIDesc: "Ollama Cloud (Cloud-hosted open models, ollama.com)"},
	{Slug: "arcee", Label: "Arcee AI", TUIDesc: "Arcee AI (Trinity models, direct API)"},
	{Slug: "gmi", Label: "GMI Cloud", TUIDesc: "GMI Cloud (Multi-model direct API)"},
	{Slug: "kilocode", Label: "Kilo Code", TUIDesc: "Kilo Code (Kilo Gateway API)"},
	{Slug: "opencode-zen", Label: "OpenCode Zen", TUIDesc: "OpenCode Zen (Curated models, pay-as-you-go)"},
	{Slug: "opencode-go", Label: "OpenCode Go", TUIDesc: "OpenCode Go (Open models subscription)"},
	{Slug: "bedrock", Label: "AWS Bedrock", TUIDesc: "AWS Bedrock (Claude, Nova, Llama, DeepSeek; IAM or API key)"},
	{Slug: "azure-foundry", Label: "Azure Foundry", TUIDesc: "Azure Foundry (OpenAI-style or Anthropic-style endpoint, your Azure AI deployment)"},
	{Slug: "ai-gateway", Label: "Vercel AI Gateway", TUIDesc: "Vercel AI Gateway (Multi-model aggregator)"},
	{Slug: "qwen-oauth", Label: "Qwen OAuth (Portal)", TUIDesc: "Qwen OAuth (Reuses local Qwen CLI login)"},
}

var builtinSlugs = func() map[string]bool {
	out := make(map[string]bool, len(builtinCanonical))
	for _, entry := range builtinCanonical {
		out[entry.Slug] = true
	}
	return out
}()

var builtinGroups = []ProviderGroup{
	{"kimi", "Kimi / Moonshot", "Coding Plan, Moonshot global & China endpoints", []string{"kimi-coding", "kimi-coding-cn"}},
	{"minimax", "MiniMax", "Global, OAuth Coding Plan & China endpoints", []string{"minimax", "minimax-oauth", "minimax-cn"}},
	{"xai", "xAI Grok", "Direct API or SuperGrok / Premium+ OAuth", []string{"xai", "xai-oauth"}},
	{"google", "Google Gemini", "Google AI Studio (API key)", []string{"gemini"}},
	{"openai", "OpenAI", "ChatGPT/Codex subscription or direct OpenAI API", []string{"openai-codex", "openai-api"}},
	{"qwen", "Qwen", "Qwen Cloud / DashScope, Coding Plan, Token Plan & Qwen CLI OAuth", []string{"alibaba", "alibaba-cn", "alibaba-coding-plan", "alibaba-coding-plan-cn", "alibaba-token-plan", "alibaba-token-plan-cn", "qwen-oauth"}},
	{"opencode", "OpenCode", "Zen pay-as-you-go or Go subscription", []string{"opencode-zen", "opencode-go"}},
	{"tencent", "Tencent Hy", "Hy4 / Hy3 via TokenHub & TokenPlan", []string{"tencent-tokenhub", "tencent-tokenplan"}},
}

var builtinAliasRows = [][2]string{
	{"glm", "zai"}, {"z-ai", "zai"}, {"z.ai", "zai"}, {"zhipu", "zai"}, {"google", "gemini"},
	{"google-gemini", "gemini"}, {"google-ai-studio", "gemini"}, {"google-vertex", "vertex"}, {"vertex-ai", "vertex"},
	{"gcp-vertex", "vertex"}, {"vertexai", "vertex"}, {"kimi", "kimi-coding"}, {"moonshot", "kimi-coding"},
	{"kimi-cn", "kimi-coding-cn"}, {"moonshot-cn", "kimi-coding-cn"}, {"step", "stepfun"},
	{"stepfun-coding-plan", "stepfun"}, {"arcee-ai", "arcee"}, {"arceeai", "arcee"}, {"gmi-cloud", "gmi"},
	{"gmicloud", "gmi"}, {"fireworks-ai", "fireworks"}, {"fw", "fireworks"}, {"actual-computer", "actual"},
	{"actualcomputer", "actual"}, {"aci", "actual"}, {"nebius", "nebius-token-factory"},
	{"nebius-tokenfactory", "nebius-token-factory"}, {"nebius-tf", "nebius-token-factory"},
	{"token-factory", "nebius-token-factory"}, {"tokenfactory", "nebius-token-factory"},
	{"minimax-china", "minimax-cn"}, {"minimax_cn", "minimax-cn"}, {"minimax-portal", "minimax-oauth"},
	{"minimax-global", "minimax-oauth"}, {"minimax_oauth", "minimax-oauth"}, {"claude", "anthropic"},
	{"claude-code", "anthropic"}, {"deep-seek", "deepseek"}, {"opencode", "opencode-zen"}, {"zen", "opencode-zen"},
	{"go", "opencode-go"}, {"opencode-go-sub", "opencode-go"}, {"aigateway", "ai-gateway"}, {"vercel", "ai-gateway"},
	{"vercel-ai-gateway", "ai-gateway"}, {"kilo", "kilocode"}, {"kilo-code", "kilocode"},
	{"kilo-gateway", "kilocode"}, {"dashscope", "alibaba"}, {"aliyun", "alibaba"}, {"qwen", "alibaba"},
	{"alibaba-cloud", "alibaba"}, {"qwen-portal", "qwen-oauth"}, {"hf", "huggingface"},
	{"hugging-face", "huggingface"}, {"huggingface-hub", "huggingface"}, {"novita-ai", "novita"},
	{"novitaai", "novita"}, {"mimo", "xiaomi"}, {"xiaomi-mimo", "xiaomi"}, {"tencent", "tencent-tokenhub"},
	{"tokenhub", "tencent-tokenhub"}, {"tencent-cloud", "tencent-tokenhub"}, {"tencentmaas", "tencent-tokenhub"},
	{"tokenplan", "tencent-tokenplan"}, {"tencent-lkeap", "tencent-tokenplan"}, {"aws", "bedrock"},
	{"aws-bedrock", "bedrock"}, {"amazon-bedrock", "bedrock"}, {"amazon", "bedrock"}, {"grok", "xai"},
	{"grok-oauth", "xai-oauth"}, {"xai-oauth", "xai-oauth"}, {"x-ai-oauth", "xai-oauth"},
	{"xai-grok-oauth", "xai-oauth"}, {"x-ai", "xai"}, {"x.ai", "xai"}, {"nim", "nvidia"}, {"nvidia-nim", "nvidia"},
	{"build-nvidia", "nvidia"}, {"nemotron", "nvidia"}, {"lmstudio", "lmstudio"}, {"lm-studio", "lmstudio"},
	{"lm_studio", "lmstudio"}, {"chatgpt", "openai-codex"}, {"chatgpt-codex", "openai-codex"},
	{"ollama", "custom"}, {"ollama_cloud", "ollama-cloud"}, {"local", "custom"}, {"vllm", "custom"},
}

var builtinAliases = func() map[string]string {
	out := make(map[string]string, len(builtinAliasRows))
	for _, row := range builtinAliasRows {
		out[row[0]] = row[1]
	}
	return out
}()

var (
	silentDefaultProviders   = []string{providers.ProviderPortal, "openrouter"}
	builtinAggregators       = []string{providers.ProviderPortal, "openrouter", "ai-gateway", "kilocode"}
	liveFirstPickerProviders = []string{"opencode-zen", "opencode-go", "meta-ai"}
	openAIFastModePrefixes   = []string{"gpt-", "o1", "o3", "o4"}
	openAIUltrafastModels    = []string{"gpt-6-astra", "gpt-6.1-sol"}
	modelsDevPreferred       = []string{
		"opencode-go", "opencode-zen", "kilocode", "fireworks", "mistral", "togetherai", "cohere",
		"perplexity", "groq", "nvidia", "huggingface", "zai", "gemini", "google", "xai", "xai-oauth",
	}
	retiredAliases = map[string][]string{"deepseek": {"deepseek-chat", "deepseek-reasoner"}}
)

var modelAliases = []aliasRow{
	{"sonnet", ModelIdentity{"anthropic", "claude-sonnet"}},
	{"opus", ModelIdentity{"anthropic", "claude-opus"}},
	{"haiku", ModelIdentity{"anthropic", "claude-haiku"}},
	{"claude", ModelIdentity{"anthropic", "claude"}},
	{"gpt5", ModelIdentity{"openai", "gpt-5"}},
	{"gpt", ModelIdentity{"openai", "gpt"}},
	{"codex", ModelIdentity{"openai", "codex"}},
	{"o3", ModelIdentity{"openai", "o3"}},
	{"o4", ModelIdentity{"openai", "o4"}},
	{"gemini", ModelIdentity{"google", "gemini"}},
	{"deepseek", ModelIdentity{"deepseek", "deepseek-chat"}},
	{"grok", ModelIdentity{"x-ai", "grok"}},
	{"llama", ModelIdentity{"meta-llama", "llama"}},
	{"qwen", ModelIdentity{"qwen", "qwen"}},
	{"minimax", ModelIdentity{"minimax", "minimax"}},
	{"nemotron", ModelIdentity{"nvidia", "nemotron"}},
	{"kimi", ModelIdentity{"moonshotai", "kimi"}},
	{"glm", ModelIdentity{"z-ai", "glm"}},
	{"step", ModelIdentity{"stepfun", "step"}},
	{"mimo", ModelIdentity{"xiaomi", "mimo"}},
	{"trinity", ModelIdentity{"arcee-ai", "trinity"}},
}

func ModelAlias(name string) (ModelIdentity, bool) {
	for _, row := range modelAliases {
		if row.alias == name {
			return row.identity, true
		}
	}
	return ModelIdentity{}, false
}

type providerTables struct {
	canonical   []providers.CanonicalEntry
	labels      map[string]string
	groups      []ProviderGroup
	slugGroup   map[string]string
	aggregators map[string]bool
	aliases     map[string]string
	aliasOrder  []string
	pluginRows  []string
}

func buildProviderTables(profiles []providers.Profile) *providerTables {
	t := &providerTables{
		canonical:   slices.Clone(builtinCanonical),
		labels:      map[string]string{},
		slugGroup:   map[string]string{},
		aggregators: map[string]bool{},
		aliases:     map[string]string{},
	}
	for _, entry := range builtinCanonical {
		t.labels[entry.Slug] = entry.Label
	}
	t.labels["custom"] = customProviderLabel
	for _, group := range builtinGroups {
		t.groups = append(t.groups, ProviderGroup{ID: group.ID, Label: group.Label, Description: group.Description, Members: slices.Clone(group.Members)})
		for _, member := range group.Members {
			t.slugGroup[member] = group.ID
		}
	}
	for _, name := range builtinAggregators {
		t.aggregators[name] = true
	}
	slugs := map[string]bool{}
	for slug := range builtinSlugs {
		slugs[slug] = true
	}
	var pending []*providers.Spec
	for _, p := range profiles {
		spec := p.Spec()
		if !slugs[spec.Name] {
			pending = append(pending, spec)
		}
	}
	for len(pending) > 0 {
		names := map[string]bool{}
		for _, spec := range pending {
			names[spec.Name] = true
		}
		var ready, waiting []*providers.Spec
		for _, spec := range pending {
			if names[spec.Picker.After] {
				waiting = append(waiting, spec)
			} else {
				ready = append(ready, spec)
			}
		}
		if len(ready) == 0 {
			ready, waiting = pending, nil
		}
		for _, spec := range ready {
			t.admit(spec)
		}
		pending = waiting
	}
	for _, p := range profiles {
		spec := p.Spec()
		if !spec.Picker.CatalogAliases || builtinSlugs[spec.Name] {
			continue
		}
		for _, alias := range spec.Aliases {
			if _, builtin := builtinAliases[alias]; builtin {
				continue
			}
			if _, taken := t.aliases[alias]; !taken {
				t.aliases[alias] = spec.Name
				t.aliasOrder = append(t.aliasOrder, alias)
			}
		}
	}
	for _, p := range profiles {
		spec := p.Spec()
		if len(spec.StaticModels) == 0 {
			continue
		}
		if _, builtin := staticRowIndex[spec.Name]; builtin || slices.Contains(t.pluginRows, spec.Name) {
			continue
		}
		t.pluginRows = append(t.pluginRows, spec.Name)
	}
	return t
}

func (t *providerTables) admit(spec *providers.Spec) {
	label := spec.DisplayName
	if label == "" {
		label = spec.Name
	}
	desc := spec.Description
	if desc == "" {
		desc = label + " (direct API)"
	}
	entry := providers.CanonicalEntry{Slug: spec.Name, Label: label, TUIDesc: desc}
	at := len(t.canonical)
	if i := slices.IndexFunc(t.canonical, func(e providers.CanonicalEntry) bool { return e.Slug == spec.Picker.After }); i >= 0 {
		at = i + 1
	}
	t.canonical = slices.Insert(t.canonical, at, entry)
	t.labels[spec.Name] = label
	if g := spec.Picker.Group; g != nil && g.ID != "" {
		i := slices.IndexFunc(t.groups, func(group ProviderGroup) bool { return group.ID == g.ID })
		if i < 0 {
			t.groups = append(t.groups, ProviderGroup{ID: g.ID, Label: g.Label, Description: g.Description})
			i = len(t.groups) - 1
		}
		if !slices.Contains(t.groups[i].Members, spec.Name) {
			t.groups[i].Members = append(t.groups[i].Members, spec.Name)
		}
		t.slugGroup[spec.Name] = g.ID
	}
	if spec.Picker.Aggregator {
		t.aggregators[spec.Name] = true
	}
}

func (c *Catalog) tables(ctx context.Context) *providerTables {
	key, generation := c.reg.BoundKey(ctx)
	c.st.mu.Lock()
	if c.st.tables != nil && c.st.tablesKey == key && c.st.tablesGeneration == generation {
		t := c.st.tables
		c.st.mu.Unlock()
		return t
	}
	c.st.mu.Unlock()
	built := buildProviderTables(c.reg.All(ctx))
	c.st.mu.Lock()
	c.st.tables, c.st.tablesKey, c.st.tablesGeneration = built, key, generation
	c.st.mu.Unlock()
	return built
}

func (c *Catalog) providerAlias(ctx context.Context, name string) (string, bool) {
	if canonical, ok := builtinAliases[name]; ok {
		return canonical, true
	}
	canonical, ok := c.tables(ctx).aliases[name]
	return canonical, ok
}

func (c *Catalog) aliasOrSelf(ctx context.Context, name string) string {
	if canonical, ok := c.providerAlias(ctx, name); ok {
		return canonical
	}
	return name
}

func (c *Catalog) normalizeProvider(ctx context.Context, provider string) string {
	if provider == "" {
		provider = "openrouter"
	}
	return c.aliasOrSelf(ctx, strings.ToLower(strings.TrimSpace(provider)))
}

func (c *Catalog) NormalizeProvider(ctx context.Context, provider string) string {
	return c.normalizeProvider(c.scoped(ctx), provider)
}

func (c *Catalog) ProviderAliases(ctx context.Context) map[string]string {
	ctx = c.scoped(ctx)
	out := make(map[string]string, len(builtinAliases))
	for alias, canonical := range builtinAliases {
		out[alias] = canonical
	}
	for alias, canonical := range c.tables(ctx).aliases {
		out[alias] = canonical
	}
	return out
}

func (c *Catalog) ProviderLabel(ctx context.Context, provider string) string {
	ctx = c.scoped(ctx)
	if provider == "" {
		provider = "openrouter"
	}
	original := strings.TrimSpace(provider)
	normalized := strings.ToLower(original)
	if normalized == "auto" {
		return "Auto"
	}
	normalized = c.normalizeProvider(ctx, normalized)
	if label, ok := c.tables(ctx).labels[normalized]; ok {
		return label
	}
	if original == "" {
		return "OpenRouter"
	}
	return original
}

func (c *Catalog) CanonicalProviders(ctx context.Context) []providers.CanonicalEntry {
	return slices.Clone(c.tables(c.scoped(ctx)).canonical)
}

func (c *Catalog) listedProviders(ctx context.Context) []providers.CanonicalEntry {
	hidden := c.reg.Unlisted(ctx, c.auth)
	out := []providers.CanonicalEntry{}
	for _, entry := range c.tables(ctx).canonical {
		if !hidden[entry.Slug] {
			out = append(out, entry)
		}
	}
	return out
}

func (c *Catalog) ListedProviders(ctx context.Context) []providers.CanonicalEntry {
	return c.listedProviders(c.scoped(ctx))
}

func (c *Catalog) ProviderGroups(ctx context.Context) []ProviderGroup {
	groups := c.tables(c.scoped(ctx)).groups
	out := make([]ProviderGroup, 0, len(groups))
	for _, group := range groups {
		out = append(out, ProviderGroup{ID: group.ID, Label: group.Label, Description: group.Description, Members: slices.Clone(group.Members)})
	}
	return out
}

func (c *Catalog) ProviderGroupFor(ctx context.Context, slug string) string {
	return c.tables(c.scoped(ctx)).slugGroup[strings.ToLower(strings.TrimSpace(slug))]
}

func (c *Catalog) GroupProviders(ctx context.Context, slugs []string) []PickerRow {
	t := c.tables(c.scoped(ctx))
	present := map[string]bool{}
	for _, slug := range slugs {
		present[slug] = true
	}
	rows := []PickerRow{}
	seen := map[string]bool{}
	emitted := map[string]bool{}
	for _, slug := range slugs {
		s := strings.ToLower(strings.TrimSpace(slug))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		gid := t.slugGroup[s]
		if gid == "" {
			rows = append(rows, PickerRow{Slug: s})
			continue
		}
		if emitted[gid] {
			continue
		}
		emitted[gid] = true
		group := t.groups[slices.IndexFunc(t.groups, func(g ProviderGroup) bool { return g.ID == gid })]
		members := []string{}
		for _, member := range group.Members {
			if present[member] {
				members = append(members, member)
			}
		}
		if len(members) == 0 {
			members = []string{s}
		}
		if len(members) <= 1 {
			rows = append(rows, PickerRow{Slug: members[0]})
			continue
		}
		rows = append(rows, PickerRow{Group: true, GroupID: gid, Label: group.Label, Description: group.Description, Members: members})
	}
	return rows
}

func (c *Catalog) isAggregatorProvider(ctx context.Context, provider string) bool {
	return c.tables(ctx).aggregators[provider]
}

func (c *Catalog) xaiCurated(ctx context.Context) []string {
	c.st.mu.Lock()
	if c.st.xai != nil {
		ids := c.st.xai
		c.st.mu.Unlock()
		return ids
	}
	c.st.mu.Unlock()
	ids := xaiFinalizeCatalog(xaiStaticFallback)
	if reg := c.modelsDevRegistry(ctx, false); reg != nil {
		if xai := reg.providers["xai"]; xai != nil && xai.hasModels && len(xai.order) > 0 {
			sorted := slices.Clone(xai.order)
			sort.Strings(sorted)
			ids = xaiFinalizeCatalog(sorted)
		}
	}
	c.st.mu.Lock()
	if c.st.xai == nil {
		c.st.xai = ids
	}
	ids = c.st.xai
	c.st.mu.Unlock()
	return ids
}

func (c *Catalog) builtinStaticRow(ctx context.Context, provider string) ([]string, bool) {
	if slices.Contains(xaiRows, provider) {
		return c.xaiCurated(ctx), true
	}
	i, ok := staticRowIndex[provider]
	if !ok {
		return nil, false
	}
	return staticRows[i].models, true
}

func (c *Catalog) staticCatalog(ctx context.Context, provider string) []string {
	if row, ok := c.builtinStaticRow(ctx, provider); ok {
		return row
	}
	if provider == "" {
		return nil
	}
	if p, ok := c.reg.Get(ctx, provider); ok {
		return p.Spec().StaticModels
	}
	return nil
}

func (c *Catalog) StaticCatalog(ctx context.Context, provider string) []string {
	return slices.Clone(c.staticCatalog(c.scoped(ctx), provider))
}

func (c *Catalog) staticCatalogProviders(ctx context.Context) []string {
	out := make([]string, 0, len(staticRows))
	for _, row := range staticRows {
		out = append(out, row.provider)
	}
	return append(out, c.tables(ctx).pluginRows...)
}
