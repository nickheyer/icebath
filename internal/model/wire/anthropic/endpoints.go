package anthropic

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	DefaultReadTimeout = 900 * time.Second
	ConnectTimeout     = 10 * time.Second
	azureAPIVersion    = "2025-04-15"
)

type AuthStyle string

const (
	StyleKimi   AuthStyle = "kimi"
	StyleBearer AuthStyle = "bearer"
	StyleAPIKey AuthStyle = "api_key"
	StyleOAuth  AuthStyle = "oauth"
)

const (
	RouteKimi       = "kimi"
	RouteDeepSeek   = "deepseek"
	RouteNative     = "native"
	RouteMiniMax    = "minimax"
	RouteThirdParty = "third_party"
)

var minimaxPrefixes = []string{"https://api.minimax.io/anthropic", "https://api.minimaxi.com/anthropic"}

var kimiModelPrefixes = []string{"kimi-", "kimi_", "moonshot-", "moonshot_", "k1.", "k1-", "k2.", "k2-", "k25", "k2.5", "k3.", "k3-"}

var deepseekThinkingPrefixes = []string{"deepseek-r", "deepseek-v4", "deepseek_v4", "deepseek-pro", "deepseek_pro", "deepseek-flash", "deepseek_flash"}

var (
	claudeVersionRE = regexp.MustCompile(`(?i)claude[-_.](opus|sonnet|haiku|fable|mythos)[-_.](\d+)`)
	claude3RE       = regexp.MustCompile(`(?i)claude[-_.]?3`)
	trailingV1RE    = regexp.MustCompile(`/v1/?$`)
)

func lowerURL(baseURL string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func splitURL(baseURL string) (host, path string) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return "", ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	} else {
		raw = strings.TrimPrefix(raw, "//")
	}
	authority := raw
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		authority = raw[:i]
		path = raw[i:]
		if j := strings.IndexAny(path, "?#"); j >= 0 {
			path = path[:j]
		}
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if strings.HasPrefix(authority, "[") {
		if end := strings.Index(authority, "]"); end > 0 {
			authority = authority[1:end]
		}
	} else if i := strings.LastIndex(authority, ":"); i >= 0 {
		authority = authority[:i]
	}
	return strings.TrimRight(strings.ToLower(authority), "."), path
}

func hostname(baseURL string) string {
	host, _ := splitURL(baseURL)
	return host
}

func hostMatches(baseURL, domain string) bool {
	host := hostname(baseURL)
	return host != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func IsThirdParty(baseURL string) bool {
	normalized := lowerURL(baseURL)
	return normalized != "" && !strings.Contains(normalized, "anthropic.com")
}

func isKimiCoding(baseURL string) bool {
	return strings.HasPrefix(lowerURL(baseURL), "https://api.kimi.com/coding")
}

func isOpenCode(baseURL string) bool { return hostMatches(baseURL, "opencode.ai") }

func modelTail(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}

func IsKimiFamilyModel(model string) bool {
	m := modelTail(model)
	return m != "" && (m == "k3" || hasAnyPrefix(m, kimiModelPrefixes))
}

func isKimiFamilyEndpoint(baseURL, model string) bool {
	return isKimiCoding(baseURL) || hostMatches(baseURL, "api.kimi.com") || hostMatches(baseURL, "moonshot.ai") ||
		hostMatches(baseURL, "moonshot.cn") || IsKimiFamilyModel(model)
}

func isDeepSeekThinkingModel(model string) bool {
	m := modelTail(model)
	return m != "" && hasAnyPrefix(m, deepseekThinkingPrefixes)
}

func isDeepSeekEndpoint(baseURL string) bool {
	return hostMatches(baseURL, "api.deepseek.com") && strings.Contains(lowerURL(baseURL), "/anthropic")
}

func IsMiniMax(baseURL string) bool { return hasAnyPrefix(lowerURL(baseURL), minimaxPrefixes) }

func requiresBearer(baseURL string, portal bool) bool {
	normalized := lowerURL(baseURL)
	return portal || hasAnyPrefix(normalized, minimaxPrefixes) || strings.Contains(normalized, "azure.com") ||
		hostMatches(normalized, "palantirfoundry.com") || hostMatches(normalized, "api.commandcode.ai")
}

func needsContext1MBeta(baseURL string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(baseURL)), "azure.com")
}

func IsAzureEndpoint(baseURL string) bool {
	host, path := splitURL(baseURL)
	if host == "" {
		return false
	}
	padded := "." + host + "."
	azure := strings.Contains(padded, ".services.ai.azure.") || strings.Contains(padded, ".openai.azure.")
	return azure && strings.Contains(strings.ToLower(path), "/anthropic")
}

func IsBedrockRuntime(baseURL string) bool {
	host := hostname(baseURL)
	labelled := false
	for _, label := range strings.Split(host, ".") {
		if strings.HasPrefix(label, "bedrock-runtime") {
			labelled = true
		}
	}
	return labelled && (hostMatches(host, "amazonaws.com") || hostMatches(host, "amazonaws.com.cn"))
}

func isVertex(baseURL string) bool {
	host := hostname(baseURL)
	return host == "aiplatform.googleapis.com" || strings.HasSuffix(host, "-aiplatform.googleapis.com")
}

func isClaudePlatform(baseURL string) bool {
	return IsBedrockRuntime(baseURL) || isVertex(baseURL) || IsAzureEndpoint(baseURL)
}

func IsNativeHost(baseURL string) bool {
	return strings.TrimSpace(baseURL) == "" || hostname(baseURL) == "api.anthropic.com"
}

func IsOAuthToken(key string) bool {
	if key == "" || hasAnyPrefix(key, []string{"sk-ant-api", "sk-ant-usr", "sk-ant-svc", "sk-ant-admin"}) {
		return false
	}
	return hasAnyPrefix(key, []string{"sk-ant-", "eyJ", "cc-"})
}

func NormalizeBaseURL(baseURL string) string {
	return trailingV1RE.ReplaceAllString(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "")
}

func Style(apiKey, baseURL string, portal bool) AuthStyle {
	switch {
	case isKimiCoding(baseURL):
		return StyleKimi
	case requiresBearer(NormalizeBaseURL(baseURL), portal):
		return StyleBearer
	case IsThirdParty(baseURL):
		return StyleAPIKey
	case IsOAuthToken(apiKey):
		return StyleOAuth
	}
	return StyleAPIKey
}

type keyAuth struct {
	key    string
	bearer bool
}

func KeyAuth(apiKey, baseURL string, portal bool) wire.Authorizer {
	style := Style(apiKey, baseURL, portal)
	return keyAuth{key: apiKey, bearer: style == StyleBearer || style == StyleOAuth}
}

func setCredential(r *http.Request, token string, bearer bool) {
	if bearer {
		r.Header.Del("X-Api-Key")
		r.Header.Set("Authorization", "Bearer "+token)
		return
	}
	r.Header.Del("Authorization")
	r.Header.Set("X-Api-Key", token)
}

func (a keyAuth) Authorize(_ context.Context, r *http.Request, _ []byte) error {
	setCredential(r, a.key, a.bearer)
	return nil
}

func (keyAuth) String() string { return "anthropic credential" }

func (keyAuth) GoString() string { return "anthropic credential" }

type tokenAuth struct {
	source func(ctx context.Context) (string, error)
	native bool
}

func TokenAuth(source func(ctx context.Context) (string, error), baseURL string) wire.Authorizer {
	return tokenAuth{source: source, native: IsNativeHost(baseURL)}
}

func (a tokenAuth) Authorize(ctx context.Context, r *http.Request, _ []byte) error {
	token, err := a.source(ctx)
	if err != nil {
		return err
	}
	setCredential(r, token, !(a.native && !IsOAuthToken(token)))
	return nil
}

func (tokenAuth) String() string { return "anthropic token source" }

func (tokenAuth) GoString() string { return "anthropic token source" }

func claudeFamilyVersion(model string) (family string, major, minor int, ok bool) {
	model = strings.TrimSpace(model)
	loc := claudeVersionRE.FindStringSubmatchIndex(model)
	if loc == nil {
		return "", 0, 0, false
	}
	family = strings.ToLower(model[loc[2]:loc[3]])
	major, err := strconv.Atoi(model[loc[4]:loc[5]])
	if err != nil {
		return "", 0, 0, false
	}
	rest := model[loc[1]:]
	if len(rest) >= 2 && strings.ContainsRune("-_.", rune(rest[0])) {
		for width := 2; width >= 1; width-- {
			if len(rest) < 1+width {
				continue
			}
			digits := rest[1 : 1+width]
			if strings.Trim(digits, "0123456789") != "" {
				continue
			}
			after := rest[1+width:]
			if after == "" || strings.ContainsRune("-_.", rune(after[0])) {
				minor, _ = strconv.Atoi(digits)
				break
			}
		}
	}
	return family, major, minor, true
}

func isClaude3(model string) bool {
	for _, loc := range claude3RE.FindAllStringIndex(model, -1) {
		if loc[1] >= len(model) || model[loc[1]] < '0' || model[loc[1]] > '9' {
			return true
		}
	}
	return false
}

func ModelPreservesPriorThinking(model string) bool {
	if !strings.Contains(strings.ToLower(model), "claude") || isClaude3(model) {
		return false
	}
	family, major, minor, ok := claudeFamilyVersion(model)
	if !ok {
		return true
	}
	fromMajor, fromMinor := 5, 0
	switch family {
	case "opus":
		fromMajor, fromMinor = 4, 5
	case "sonnet":
		fromMajor, fromMinor = 4, 6
	case "haiku":
		fromMajor, fromMinor = 5, 5
	}
	return major > fromMajor || (major == fromMajor && minor >= fromMinor)
}

func isOffAnthropic(baseURL string, portal bool) bool { return IsThirdParty(baseURL) && !portal }

func ThinkingRoute(baseURL, model string, portal bool) string {
	switch {
	case isKimiFamilyEndpoint(baseURL, model):
		return RouteKimi
	case isDeepSeekEndpoint(baseURL) || (isOffAnthropic(baseURL, portal) && isDeepSeekThinkingModel(model)):
		return RouteDeepSeek
	case !isOffAnthropic(baseURL, portal) || isClaudePlatform(baseURL):
		return RouteNative
	case IsMiniMax(baseURL):
		return RouteMiniMax
	}
	return RouteThirdParty
}

func RouteSignsThinking(route string) bool { return route == RouteNative || route == RouteMiniMax }

func RouteReplaysPriorThinking(route, model string) bool {
	return route == RouteMiniMax || (route == RouteNative && ModelPreservesPriorThinking(model))
}

func PreservesPriorThinking(baseURL, model string, portal bool) bool {
	return RouteReplaysPriorThinking(ThinkingRoute(baseURL, model, portal), model)
}
