package transport

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// MiniMax's Anthropic endpoints, global and China
var minimaxPrefixes = []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"}

var trailingV1 = regexp.MustCompile(`/v1/?$`)

// Base URL without surrounding space, a trailing slash or a trailing /v1
func normalized(base string) string {
	return trailingV1.ReplaceAllString(strings.TrimRight(strings.TrimSpace(base), "/"), "")
}

func lower(base string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(base), "/"))
}

// Any endpoint off anthropic.com, which takes its own keys and is no subscription's
func thirdParty(base string) bool {
	n := lower(base)
	return n != "" && !strings.Contains(n, "anthropic.com")
}

func nativeHost(base string) bool {
	return strings.TrimSpace(base) == "" || provider.BaseHost(base) == "api.anthropic.com"
}

func kimiCoding(base string) bool {
	return strings.HasPrefix(lower(base), "https://api.kimi.com/coding")
}

func minimax(base string) bool {
	n := lower(base)
	return slices.ContainsFunc(minimaxPrefixes, func(p string) bool { return strings.HasPrefix(n, p) })
}

// Endpoints that take the key as a bearer token rather than x-api-key
func bearer(base string) bool {
	n := lower(base)
	return strings.Contains(n, "azure.com") || minimax(base) || provider.BaseHostMatches(n, "palantirfoundry.com") ||
		provider.BaseHostMatches(n, "api.commandcode.ai")
}

// Azure's Anthropic endpoints, which take an api-version query and gate the 1M window behind its beta
func azureAnthropic(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return false
	}
	host := "." + strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") + "."
	return (strings.Contains(host, ".services.ai.azure.") || strings.Contains(host, ".openai.azure.")) && strings.Contains(strings.ToLower(u.Path), "/anthropic")
}

func bedrockRuntime(base string) bool {
	host := provider.BaseHost(base)
	labeled := slices.ContainsFunc(strings.Split(host, "."), func(label string) bool { return strings.HasPrefix(label, "bedrock-runtime") })
	return labeled && (provider.BaseHostMatches(host, "amazonaws.com") || provider.BaseHostMatches(host, "amazonaws.com.cn"))
}

func vertexEndpoint(base string) bool {
	host := provider.BaseHost(base)
	return host == "aiplatform.googleapis.com" || strings.HasSuffix(host, "-aiplatform.googleapis.com")
}

// Cloud platforms serving Claude with Anthropic's signed thinking
func claudePlatform(base string) bool {
	return bedrockRuntime(base) || vertexEndpoint(base) || azureAnthropic(base)
}

// Setup tokens and OAuth tokens of a Claude subscription, never the Console's API key families
func oauthToken(key string) bool {
	if key == "" {
		return false
	}
	for _, p := range []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"} {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	return strings.HasPrefix(key, "sk-ant-") || strings.HasPrefix(key, "eyJ") || strings.HasPrefix(key, "cc-")
}

// Kimi or Moonshot Anthropic endpoints, by host or by the model
func kimiEndpoint(base, model string) bool {
	return kimiCoding(base) || provider.KimiModel(model) || slices.ContainsFunc([]string{"api.kimi.com", "moonshot.ai", "moonshot.cn"},
		func(domain string) bool { return provider.BaseHostMatches(base, domain) })
}

var deepseekThinking = []string{"deepseek-r", "deepseek-v4", "deepseek_v4", "deepseek-pro", "deepseek_pro", "deepseek-flash", "deepseek_flash"}

func deepseekModel(model string) bool {
	m := provider.BareModel(model)
	return m != "" && provider.HasPrefix(m, deepseekThinking...)
}

func deepseekEndpoint(base string) bool {
	return provider.BaseHostMatches(base, "api.deepseek.com") && strings.Contains(lower(base), "/anthropic")
}

// Versions of each family from which Anthropic keeps earlier turns' thinking in view
var (
	claude3           = regexp.MustCompile(`(?i)claude[-_.]?3(?:[^\d]|$)`)
	keepsThinkingFrom = map[string][2]int{"opus": {4, 5}, "sonnet": {4, 6}, "haiku": {5, 5}}
)

// Whether Anthropic keeps the thinking of earlier assistant turns in the model's view
func keepsPriorThinking(model string) bool {
	if !strings.Contains(strings.ToLower(model), "claude") || claude3.MatchString(model) {
		return false
	}
	family, version, ok := provider.ClaudeFamily(model)
	if !ok {
		return true
	}
	floor, known := keepsThinkingFrom[family]
	if !known {
		floor = [2]int{5, 0}
	}
	return provider.AtLeast(version, floor)
}

// How an endpoint wants thinking replayed: kimi, deepseek, native, minimax or third_party
func thinkingRoute(base, model string) string {
	switch {
	case kimiEndpoint(base, model):
		return "kimi"
	case deepseekEndpoint(base) || thirdParty(base) && deepseekModel(model):
		return "deepseek"
	case !thirdParty(base) || claudePlatform(base):
		return "native"
	case minimax(base):
		return "minimax"
	}
	return "third_party"
}

// Whether the route replays the thinking of turns before the current tool loop
func replaysPriorThinking(route, model string) bool {
	return route == "minimax" || route == "native" && keepsPriorThinking(model)
}

// Whether model ids keep their dots: those of kinds whose ids have them, and of the hosts that name their models with them
func keepsDots(spec provider.Spec, route provider.Route) bool {
	if spec.DottedModels {
		return true
	}
	base := strings.ToLower(route.BaseURL)
	host := provider.BaseHost(base)
	return strings.Contains(host, "dashscope") || provider.BaseHostMatches(base, "aliyuncs.com") || strings.Contains(host, "minimax") ||
		provider.BaseHostMatches(base, "opencode.ai") && strings.Contains(base, "/zen/") || provider.BaseHostMatches(base, "bigmodel.cn") ||
		provider.BaseHostMatches(base, "xiaomimimo.com") || provider.BaseHostMatches(base, "aiplatform.googleapis.com") || strings.HasPrefix(host, "bedrock-runtime.")
}
