package provider

import (
	"context"
	"strings"
)

// Key under which a hook context map carries the caller's context, the Go form of the Python ambient contextvars
const hookCtxKey = "ctx"

// Ported bundled model provider plugin directories, filled at init so registry variable initializers never reach them
var bundledPluginPorts = map[string]func() []Profile{}

func init() {
	bundledPluginPorts["openrouter"] = openrouterPluginProfiles
	bundledPluginPorts["router"] = routerPluginProfiles
	bundledPluginPorts["opencode-zen"] = opencodePluginProfiles
	bundledPluginPorts["custom"] = customPluginProfiles
	bundledPluginPorts["commandcode"] = commandcodePluginProfiles
	bundledPluginPorts["deepseek"] = deepseekPluginProfiles
	bundledPluginPorts["actual"] = actualPluginProfiles
	bundledPluginPorts["kimi-coding"] = kimiPluginProfiles
	bundledPluginPorts["zai"] = zaiPluginProfiles
	bundledPluginPorts["nous"] = nousPluginProfiles
	bundledPluginPorts["meta-ai"] = metaAIPluginProfiles
	bundledPluginPorts["qwen-oauth"] = qwenPluginProfiles
	bundledPluginPorts["solstice"] = solsticePluginProfiles
	bundledPluginPorts["deepinfra"] = deepinfraPluginProfiles
	bundledPluginPorts["minimax"] = minimaxPluginProfiles
	bundledPluginPorts["alibaba"] = alibabaPluginProfiles
	bundledPluginPorts["anthropic"] = anthropicPluginProfiles
	bundledPluginPorts["upstage"] = upstagePluginProfiles
	bundledPluginPorts["nebius-token-factory"] = nebiusPluginProfiles
	bundledPluginPorts["fireworks"] = fireworksPluginProfiles
	bundledPluginPorts["gemini"] = geminiPluginProfiles
	bundledPluginPorts["lmstudio"] = lmstudioPluginProfiles
	bundledPluginPorts["alibaba-coding-plan"] = alibabaCodingPluginProfiles
	bundledPluginPorts["ollama-cloud"] = ollamaCloudPluginProfiles
	bundledPluginPorts["tencent-tokenhub"] = tokenhubPluginProfiles
	bundledPluginPorts["nvidia"] = nvidiaPluginProfiles
	bundledPluginPorts["vertex"] = vertexPluginProfiles
	bundledPluginPorts["ai-gateway"] = aiGatewayPluginProfiles
	bundledPluginPorts["gmi"] = gmiPluginProfiles
	bundledPluginPorts["novita"] = novitaPluginProfiles
	bundledPluginPorts["bedrock"] = bedrockPluginProfiles
	bundledPluginPorts["azure-foundry"] = azureFoundryPluginProfiles
	bundledPluginPorts["huggingface"] = huggingfacePluginProfiles
	bundledPluginPorts["xiaomi"] = xiaomiPluginProfiles
	bundledPluginPorts["xai"] = xaiPluginProfiles
	bundledPluginPorts["kilocode"] = kilocodePluginProfiles
	bundledPluginPorts["arcee"] = arceePluginProfiles
	bundledPluginPorts["stepfun"] = stepfunPluginProfiles
	bundledPluginPorts["openai-codex"] = openaiCodexPluginProfiles
}

// Profiles of a ported bundled model provider plugin directory in register order, nil for a dir still read from rows
func bundledPluginPort(dir string) func() []Profile { return bundledPluginPorts[dir] }

// Caller context a hook context map carries, the empty background context when the caller set none
func hookCtx(kw map[string]any) context.Context {
	if c, ok := kw[hookCtxKey].(context.Context); ok && c != nil {
		return c
	}
	return context.Background()
}

// String value of a hook context key, empty for a missing or non string value
func hookString(kw map[string]any, key string) string {
	s, _ := kw[key].(string)
	return s
}

// Python dict form of a reasoning config, enabled and effort present only when set
func reasoningConfigMap(r *Reasoning) map[string]any {
	if r == nil {
		return nil
	}
	m := map[string]any{}
	if r.Enabled != nil {
		m["enabled"] = *r.Enabled
	}
	if r.Effort != "" {
		m["effort"] = r.Effort
	}
	return m
}

// Copy of a dict with its top level keys duplicated
func copyDict(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Profile built from a compact row with the given aliases
func bundledRowProfile(r profileRow, aliases ...string) Profile {
	p := r.profile()
	p.Aliases = aliases
	return p
}

// Attribution default headers resolving the user agent on first read
func attributionLazyHeaders(product string) *LazyHeaders {
	return NewLazyHeaders(HeaderEntry{Key: "HTTP-Referer", Value: attributionReferer}, HeaderEntry{Key: "X-Title", Value: attributionTitle},
		HeaderEntry{Key: "User-Agent", Fn: func() string { return UserAgent(product) }})
}

// Request reasoning shape of a hook reasoning config, unset for nil
func hookReasoningCfg(r *Reasoning) reasoningCfg {
	if r == nil {
		return reasoningCfg{}
	}
	return reasoningFromRequest(*r)
}

// Bare lowercase model id after any aggregator prefix
func flatModelName(model string) string {
	m := strings.TrimSpace(model)
	return strings.ToLower(m[strings.LastIndex(m, "/")+1:])
}

// Python list repr of strings
func pyStrListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = pyStringRepr(it)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
