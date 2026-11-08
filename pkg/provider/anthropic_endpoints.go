package provider

import (
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var minimaxAnthropicPrefixes = []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"}

// Parses a base URL that may lack a scheme, nil when empty
func parseBaseURL(raw string) *url.URL {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}

// Lowercased hostname of a base URL, empty when absent
func baseURLHostname(raw string) string {
	u := parseBaseURL(raw)
	if u == nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// Reports whether the base URL host is the domain or one of its subdomains
func baseURLHostMatches(raw, domain string) bool {
	host := baseURLHostname(raw)
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func normalizeBaseURLText(raw string) string { return strings.TrimSpace(raw) }

func normalizedLowerBaseURL(raw string) string {
	return strings.ToLower(strings.TrimRight(normalizeBaseURLText(raw), "/"))
}

// Any endpoint off anthropic.com, which uses its own keys and skips OAuth detection
func isThirdPartyAnthropicEndpoint(baseURL string) bool {
	n := normalizedLowerBaseURL(baseURL)
	return n != "" && !strings.Contains(n, "anthropic.com")
}

func isKimiCodingEndpoint(baseURL string) bool {
	return strings.HasPrefix(normalizedLowerBaseURL(baseURL), "https://api.kimi.com/coding")
}

func isOpenCodeEndpoint(baseURL string) bool { return baseURLHostMatches(baseURL, "opencode.ai") }

var kimiFamilyModelPrefixes = []string{"kimi-", "kimi_", "moonshot-", "moonshot_", "k1.", "k1-", "k2.", "k2-", "k25", "k2.5", "k3.", "k3-"}

func modelNameIsKimiFamily(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		return false
	}
	if m == "k3" {
		return true
	}
	for _, p := range kimiFamilyModelPrefixes {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// Kimi or Moonshot Anthropic Messages endpoints by host or by model family
func isKimiFamilyEndpoint(baseURL, model string) bool {
	if isKimiCodingEndpoint(baseURL) || modelNameIsKimiFamily(model) {
		return true
	}
	for _, d := range []string{"api.kimi.com", "moonshot.ai", "moonshot.cn"} {
		if baseURLHostMatches(baseURL, d) {
			return true
		}
	}
	return false
}

var deepseekThinkingPrefixes = []string{"deepseek-r", "deepseek-v4", "deepseek_v4", "deepseek-pro", "deepseek_pro", "deepseek-flash", "deepseek_flash"}

func modelNameIsDeepSeekThinking(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return false
	}
	for _, p := range deepseekThinkingPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func isDeepSeekAnthropicEndpoint(baseURL string) bool {
	return baseURLHostMatches(baseURL, "api.deepseek.com") && strings.Contains(normalizedLowerBaseURL(baseURL), "/anthropic")
}

// Nous Portal by production host or the NOUS_INFERENCE_BASE_URL override host
func isNousPortalEndpoint(baseURL string) bool {
	if baseURLHostMatches(baseURL, "inference-api.nousresearch.com") {
		return true
	}
	override := strings.TrimSpace(os.Getenv("NOUS_INFERENCE_BASE_URL"))
	if override == "" {
		return false
	}
	oh := baseURLHostname(override)
	return oh != "" && baseURLHostname(baseURL) == oh
}

// Providers wanting Authorization Bearer instead of x-api-key
func requiresBearerAuth(baseURL string) bool {
	n := normalizedLowerBaseURL(baseURL)
	if isNousPortalEndpoint(baseURL) || strings.Contains(n, "azure.com") {
		return true
	}
	for _, p := range minimaxAnthropicPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return baseURLHostMatches(n, "palantirfoundry.com") || baseURLHostMatches(n, "api.commandcode.ai")
}

func baseURLNeedsContext1MBeta(baseURL string) bool {
	return strings.Contains(strings.ToLower(normalizeBaseURLText(baseURL)), "azure.com")
}

func isMinimaxAnthropicEndpoint(baseURL string) bool {
	n := normalizedLowerBaseURL(baseURL)
	for _, p := range minimaxAnthropicPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// Azure hosted Anthropic Messages endpoints serving an anthropic path
func isAzureAnthropicEndpoint(baseURL string) bool {
	u := parseBaseURL(baseURL)
	if u == nil {
		return false
	}
	host := "." + strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") + "."
	azure := strings.Contains(host, ".services.ai.azure.") || strings.Contains(host, ".openai.azure.")
	return azure && strings.Contains(strings.ToLower(u.Path), "/anthropic")
}

func isBedrockRuntimeEndpoint(baseURL string) bool {
	host := baseURLHostname(baseURL)
	labeled := false
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(label, "bedrock-runtime") {
			labeled = true
		}
	}
	return labeled && (baseURLHostMatches(host, "amazonaws.com") || baseURLHostMatches(host, "amazonaws.com.cn"))
}

func isVertexAIEndpoint(baseURL string) bool {
	host := baseURLHostname(baseURL)
	return host == "aiplatform.googleapis.com" || strings.HasSuffix(host, "-aiplatform.googleapis.com")
}

// Cloud platforms serving Claude under Anthropic signed thinking
func isClaudePlatformEndpoint(baseURL string) bool {
	return isBedrockRuntimeEndpoint(baseURL) || isVertexAIEndpoint(baseURL) || isAzureAnthropicEndpoint(baseURL)
}

// OAuth and setup token shapes, never the Console API key families
func isOAuthToken(key string) bool {
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

// IsOAuthToken reports an OAuth or setup token shape rather than a Console API key
func IsOAuthToken(key string) bool { return isOAuthToken(key) }

func isNativeAnthropicHost(baseURL string) bool {
	text := strings.TrimSpace(baseURL)
	return text == "" || baseURLHostname(text) == "api.anthropic.com"
}

// Claude Code OAuth identity applies on the native host or the anthropic provider with an OAuth credential
func anthropicRouteIsOAuth(baseURL, credential, providerName string) bool {
	if !(isNativeAnthropicHost(baseURL) || strings.ToLower(strings.TrimSpace(providerName)) == "anthropic") {
		return false
	}
	return isOAuthToken(credential)
}

var claudeVersionRe = regexp.MustCompile(`(?i)claude[-_.](opus|sonnet|haiku|fable|mythos)[-_.](\d+)(?:[-_.](\d{1,2})(?:$|[-_.]))?`)
var lastTurnOnlyRe = regexp.MustCompile(`(?i)claude[-_.]?3(?:[^\d]|$)`)

var keepAllFrom = map[string][2]int{"opus": {4, 5}, "sonnet": {4, 6}, "haiku": {5, 5}}

// Family and major minor version parsed from a Claude model id
func claudeFamilyVersion(model string) (string, [2]int, bool) {
	m := claudeVersionRe.FindStringSubmatch(strings.TrimSpace(model))
	if m == nil {
		return "", [2]int{}, false
	}
	major, _ := strconv.Atoi(m[2])
	minor := 0
	if m[3] != "" {
		minor, _ = strconv.Atoi(m[3])
	}
	return strings.ToLower(m[1]), [2]int{major, minor}, true
}

func versionAtLeast(v, floor [2]int) bool {
	return v[0] > floor[0] || (v[0] == floor[0] && v[1] >= floor[1])
}

// Whether Anthropic keeps prior assistant thinking in model visible context
func modelPreservesPriorThinking(model string) bool {
	lower := strings.ToLower(model)
	if !strings.Contains(lower, "claude") || lastTurnOnlyRe.MatchString(model) {
		return false
	}
	family, version, ok := claudeFamilyVersion(model)
	if !ok {
		return true
	}
	floor, known := keepAllFrom[family]
	if !known {
		floor = [2]int{5, 0}
	}
	return versionAtLeast(version, floor)
}

func isOffAnthropic(baseURL string) bool {
	return isThirdPartyAnthropicEndpoint(baseURL) && !isNousPortalEndpoint(baseURL)
}

// Thinking replay contract of a route, kimi deepseek native minimax or third_party
func anthropicThinkingRoute(baseURL, model string) string {
	switch {
	case isKimiFamilyEndpoint(baseURL, model):
		return "kimi"
	case isDeepSeekAnthropicEndpoint(baseURL) || (isOffAnthropic(baseURL) && modelNameIsDeepSeekThinking(model)):
		return "deepseek"
	case !isOffAnthropic(baseURL) || isClaudePlatformEndpoint(baseURL):
		return "native"
	case isMinimaxAnthropicEndpoint(baseURL):
		return "minimax"
	}
	return "third_party"
}

func routeReplaysPriorThinking(route, model string) bool {
	return route == "minimax" || (route == "native" && modelPreservesPriorThinking(model))
}
