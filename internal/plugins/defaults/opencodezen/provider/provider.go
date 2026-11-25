package provider

import (
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	opencodezenconfig "github.com/nickheyer/icebath/internal/plugins/defaults/opencodezen/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type rule struct {
	prefixes []string
	mode     provider.APIMode
}

var rules = map[string][]rule{
	opencodezenconfig.GoName: {
		{[]string{"gpt-", "grok-", "muse-spark"}, provider.ModeCodexResponses},
		{[]string{"claude-", "minimax-", "qwen", "union-alpha"}, provider.ModeAnthropicMessages},
	},
	opencodezenconfig.ZenName: {
		{[]string{"claude-", "union-alpha"}, provider.ModeAnthropicMessages},
		{[]string{"gpt-", "grok-", "muse-spark"}, provider.ModeCodexResponses},
		{[]string{"qwen"}, provider.ModeAnthropicMessages},
	},
}

var zenModels = []string{
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
}

var goModels = []string{
	"kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "kimi-k2.5", "gpt-5.6-luna", "grok-4.5", "glm-5.3",
	"glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5", "mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-pro",
	"mimo-v2-omni", "minimax-m3", "minimax-m2.7", "minimax-m2.5", "deepseek-v4-pro",
	"deepseek-v4-flash", "qwen3.8-max", "qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus",
	"qwen3.5-plus", "hy3", "hy3-preview", "muse-spark-1.2-contributor", "muse-spark-1.3-contributor",
}

type profiles struct {
	config *config.Config
}

func Register(registry *provider.Registry, cfg *config.Config) {
	registry.Register(profiles{config: cfg})
}

func (p profiles) Profiles() []provider.Profile {
	zenBase := p.config.GetString(opencodezenconfig.ZenBaseURL)
	goBase := p.config.GetString(opencodezenconfig.GoBaseURL)
	return []provider.Profile{{
		Name:               opencodezenconfig.ZenName,
		DisplayName:        "OpenCode Zen",
		BaseURL:            zenBase,
		BaseURLEnv:         opencodezenconfig.ZenBaseURLEnv,
		ModelsURL:          provider.ModelsURL(zenBase),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{opencodezenconfig.ZenAPIKeyEnv},
		Aliases:            []string{"opencode", "opencode_zen", "zen"},
		DefaultModel:       p.config.GetString(opencodezenconfig.ZenDefaultModel),
		Headers:            attribution(),
		Aggregator:         true,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		ModelsDevID:        "opencode",
		PerModelMode:       func(model string) provider.APIMode { return mode(opencodezenconfig.ZenName, model) },
		DefaultAuxModel:    "gemini-3-flash",
		StaticModels:       slices.Clone(zenModels),
	}, {
		Name:            opencodezenconfig.GoName,
		DisplayName:     "OpenCode Go",
		BaseURL:         goBase,
		BaseURLEnv:      opencodezenconfig.GoBaseURLEnv,
		ModelsURL:       provider.ModelsURL(goBase),
		Mode:            provider.ModeChatCompletions,
		Auth:            provider.AuthAPIKey,
		AuthType:        provider.AuthTypeAPIKey,
		KeyEnv:          []string{opencodezenconfig.GoAPIKeyEnv},
		Aliases:         []string{"opencode_go", "go", "opencode-go-sub"},
		DefaultModel:    p.config.GetString(opencodezenconfig.GoDefaultModel),
		Headers:         attribution(),
		Aggregator:      true,
		ModelListing:    true,
		HealthCheck:     true,
		ModelsDevID:     opencodezenconfig.GoName,
		PerModelMode:    func(model string) provider.APIMode { return mode(opencodezenconfig.GoName, model) },
		DefaultAuxModel: "glm-5",
		StaticModels:    slices.Clone(goModels),
	}}
}

func mode(family, model string) provider.APIMode {
	normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), family+"/")
	for _, r := range rules[family] {
		if slices.ContainsFunc(r.prefixes, func(prefix string) bool { return strings.HasPrefix(normalized, prefix) }) {
			return r.mode
		}
	}
	return provider.ModeChatCompletions
}

func attribution() map[string]string {
	return map[string]string{"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent", "User-Agent": "IcebathAgent/dev"}
}
