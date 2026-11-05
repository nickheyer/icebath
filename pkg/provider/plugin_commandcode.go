package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	commandcodeBase      = "https://api.commandcode.ai/provider/v1"
	commandcodeModelsURL = commandcodeBase + "/models"
)

var deepseekThinkingCapableIDs = []string{"deepseek-flash"}

// Profiles the bundled commandcode plugin registers, the chat completions and the Anthropic Messages endpoints
func commandcodePluginProfiles() []Profile {
	chat := bundledRowProfile(profileRow{name: "commandcode", display: "CommandCode", mode: ModeChatCompletions,
		desc: "CommandCode — 20+ models via OpenAI-compatible API", signup: "https://commandcode.ai/",
		keys: []string{"COMMANDCODE_API_KEY", "COMMANDCODE_BASE_URL"}, base: commandcodeBase, modelsURL: commandcodeModelsURL,
		fallbacks: []string{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "Qwen/Qwen3.7-Max", "Qwen/Qwen3.6-Plus",
			"moonshotai/Kimi-K2.6", "zai-org/GLM-5.1", "MiniMaxAI/MiniMax-M2.7", "stepfun/Step-3.5-Flash",
			"xiaomi/mimo-v2.5-pro", "google/gemini-3.5-flash", "gpt-5.5"},
	}, "commandcode-chat")
	chat.DefaultAuxModel = "deepseek/deepseek-v4-flash"
	chat.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		return commandcodeFetchModels(ctx, baseURL, timeout)
	}
	chat.BuildAPIKwargsExtrasHook = commandcodeBuildAPIKwargsExtras
	anth := bundledRowProfile(profileRow{name: "commandcode-anthropic", display: "CommandCode (Anthropic)", mode: ModeAnthropicMessages,
		desc: "CommandCode — Claude models via Anthropic Messages API", signup: "https://commandcode.ai/",
		keys: []string{"COMMANDCODE_API_KEY", "COMMANDCODE_ANTHROPIC_BASE_URL"}, base: commandcodeBase, modelsURL: commandcodeModelsURL,
		fallbacks: []string{"claude-sonnet-4-6", "claude-opus-4-7", "claude-haiku-4-5-20251001"},
	}, "commandcode-claude")
	anth.DefaultAuxModel = "claude-haiku-4-5-20251001"
	anth.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		all, ok := commandcodeFetchModels(ctx, baseURL, timeout)
		if !ok {
			return nil, false
		}
		claude := []string{}
		for _, m := range all {
			if strings.HasPrefix(m, "claude-") {
				claude = append(claude, m)
			}
		}
		return claude, true
	}
	anth.BuildAPIKwargsExtrasHook = commandcodeBuildAPIKwargsExtras
	return []Profile{chat, anth}
}

// Public unauthenticated models listing, a caller base differing from the default counting as a custom endpoint
func commandcodeFetchModels(ctx context.Context, baseURL string, timeout time.Duration) ([]string, bool) {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	data, err := modelsGetJSON(ctx, commandcodeModelsURLFor(baseURL), timeout, map[string]string{"Accept": "application/json", "User-Agent": profileUserAgent()}, nil)
	if err == nil {
		if _, isDict := data.(map[string]any); !isDict {
			err = fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(data))
		}
	}
	if err != nil {
		slog.Debug("fetch_models(commandcode): " + err.Error())
		return nil, false
	}
	items, _ := data.(map[string]any)["data"].([]any)
	ids := []string{}
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if id, present := m["id"]; present {
				ids = append(ids, pyScalarStr(id))
			}
		}
	}
	return ids, true
}

// Listing URL for a caller base, the default listing unless the base differs from the default endpoint
func commandcodeModelsURLFor(baseURL string) string {
	callerBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if callerBase != "" && callerBase != commandcodeBase {
		return callerBase + "/models"
	}
	return commandcodeModelsURL
}

// DeepSeek ids get the native DeepSeek wire controls through the registered deepseek profile, other families none
func commandcodeBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	m := strings.TrimSpace(hookString(kw, "model"))
	if !strings.HasPrefix(strings.ToLower(m), "deepseek/") {
		return map[string]any{}, map[string]any{}
	}
	native, ok := GetProviderProfile(hookCtx(kw), "deepseek")
	if !ok {
		return map[string]any{}, map[string]any{}
	}
	next := copyDict(kw)
	next["model"] = m[strings.Index(m, "/")+1:]
	return native.BuildAPIKwargsExtras(reasoning, next)
}

// Profiles the bundled deepseek plugin registers
func deepseekPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "deepseek", display: "DeepSeek", desc: "DeepSeek — native DeepSeek API",
		signup: "https://platform.deepseek.com/", keys: []string{"DEEPSEEK_API_KEY"}, base: "https://api.deepseek.com/v1",
		fallbacks: []string{"deepseek-v4-pro", "deepseek-flash"}, badFormats: []string{"json_schema"},
	}, "deepseek-chat", "deep-seek")
	p.DefaultAuxModel = "deepseek-flash"
	p.BuildAPIKwargsExtrasHook = deepseekBuildAPIKwargsExtras
	return []Profile{p}
}

// Explicit thinking toggle with the effort on V4 and version less thinking ids, an off effort disabling thinking
func deepseekBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	m := strings.ToLower(strings.TrimSpace(hookString(kw, "model")))
	versionedV4Plus := strings.HasPrefix(m, "deepseek-v") && !strings.HasPrefix(m, "deepseek-v3")
	if !versionedV4Plus && !containsStr(deepseekThinkingCapableIDs, m) {
		return map[string]any{}, map[string]any{}
	}
	c := hookReasoningCfg(reasoning)
	extras, topLevel := thinkingToggleExtras(c, deepseekV4Efforts, deepseekV4Overrides, true)
	if reasoning != nil && (c.effort == "none" || c.effort == "false" || c.effort == "disabled") {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{}
	}
	return extras, topLevel
}
