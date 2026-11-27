package provider

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"

	customconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/custom/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Hosts of servers that refuse every reasoning field of a replayed message
var noCarriers = []string{"mistral.ai", "groq.com", "cerebras.ai", "sambanova.ai", "hunyuan.cloud.tencent.com"}

// Whether the address looks like an Ollama server: its port, ollama.com, or a host named ollama
func ollama(base string) bool {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n > 65535 {
			return false
		}
		if n == 11434 {
			return true
		}
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	return host != "" && (host == "ollama.com" || strings.HasSuffix(host, ".ollama.com") || slices.Contains(strings.Split(host, "."), "ollama"))
}

// Context window to ask an Ollama server for: the setting, else the one the server reports, 0 for none
func window(call *provider.Call) int {
	if n := call.Settings.GetInt(customconfig.OllamaNumCtx); n > 0 {
		return n
	}
	if call.Window != nil {
		return call.Window()
	}
	return 0
}

// Medium reasoning unasked, none on an Ollama server that says the model doesn't think
func defaultReasoning(call *provider.Call) provider.Ask {
	if (ollama(call.Route.BaseURL) || window(call) != 0) && (call.OllamaThinks == nil || !call.OllamaThinks()) {
		return provider.Ask{}
	}
	return *provider.Effort("medium")
}

// Ollama's context window, and the effort at the top: none when off (with Ollama's think off too), Groq's default, else clamped
func shape(call *provider.Call) (map[string]any, map[string]any) {
	extra, top := map[string]any{}, map[string]any{}
	if n := window(call); n > 0 {
		extra["options"] = map[string]any{"num_ctx": n}
	}
	switch ask := call.Ask; {
	case !ask.Set:
	case ask.Off || ask.Effort == "none":
		top["reasoning_effort"] = "none"
		if ollama(call.Route.BaseURL) {
			extra["think"] = false
		}
	case ask.Effort != "" && provider.BaseHostMatches(call.Route.BaseURL, "api.groq.com"):
		top["reasoning_effort"] = "default"
	case ask.Effort != "":
		top["reasoning_effort"] = provider.ClampEffort(ask.Effort, provider.Efforts, nil)
	}
	return extra, top
}

// Reasoning fields a server of your own reads back: none for the hosts that refuse them, else reasoning_content and reasoning
func carriers(base string) []string {
	if slices.ContainsFunc(noCarriers, func(host string) bool { return provider.BaseHostMatches(base, host) }) {
		return []string{}
	}
	return []string{"reasoning_content", "reasoning"}
}

func efforts(context.Context, string) ([]string, bool) {
	return slices.Clone(provider.Efforts), true
}

// Names Perplexity's API keeps for its own tools
var perplexityReserved = []string{"web_search", "search_files", "fetch_url", "people_search", "finance_search"}
