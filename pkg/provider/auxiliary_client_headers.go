package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Default auxiliary models and endpoints of the OpenRouter, Nous and Anthropic routes
const (
	auxOpenRouterModel         = "nvidia/nemotron-3-ultra-550b-a55b:free"
	auxNousModel               = "google/gemini-3.6-flash"
	auxNousDefaultBaseURL      = "https://inference-api.nousresearch.com/v1"
	auxAnthropicDefaultBaseURL = "https://api.anthropic.com"
)

// NVIDIA NIM cloud attribution sent to build.nvidia.com traffic
var auxNvidiaNIMCloudHeaders = map[string]string{"X-BILLING-INVOKE-ORIGIN": "IcebathAgent"}

// MiniMax family hosts that serve both the Anthropic and the OpenAI surface
var (
	auxDualSurfaceAnthropicHostSuffixes = []string{"minimax.io", "minimax.chat", "minimaxi.com"}
	auxDualSurfaceAnthropicHostPrefixes = []string{"api.minimax."}
)

// Read only config load of the active profile, a seam so tests can make it fail
var auxLoadConfigReadonlyFn = func(ctx context.Context) (config.Tree, error) {
	return config.LoadReadonly(ctx, home.Dir(ctx), nil)
}

// Patched auth.json path of a test, empty to use the active profile's file
var auxAuthJSONPathOverride string

// OpenRouter attribution headers every OpenRouter call carries
func auxORHeadersBase() map[string]string {
	return map[string]string{
		"HTTP-Referer":            attributionReferer,
		"X-Title":                 attributionTitle,
		"X-OpenRouter-Categories": "productivity,cli-agent",
	}
}

// Merges model.default_headers and its model.extra_headers alias over the resolved headers, user values winning
func auxApplyUserDefaultHeaders(ctx context.Context, headers map[string]string) map[string]string {
	tree, err := auxLoadConfigReadonlyFn(ctx)
	if err != nil {
		return headers
	}
	userHeaders, _ := config.CfgGet(tree, "model", "default_headers")
	aliasHeaders, _ := config.CfgGet(tree, "model", "extra_headers")
	if alias, ok := aliasHeaders.(map[string]any); ok && len(alias) > 0 {
		merged := map[string]any{}
		if base, ok := userHeaders.(map[string]any); ok {
			for k, v := range base {
				merged[k] = v
			}
		}
		for k, v := range alias {
			merged[k] = v
		}
		userHeaders = merged
	}
	user, ok := userHeaders.(map[string]any)
	if !ok || len(user) == 0 {
		return headers
	}
	merged := map[string]string{}
	for k, v := range headers {
		merged[k] = v
	}
	for k, v := range user {
		if v != nil {
			merged[k] = pyrepr.Str(v)
		}
	}
	if len(merged) == 0 {
		return headers
	}
	return merged
}

// Resolved headers with model.default_headers and model.extra_headers merged over them, user values winning
func ApplyUserDefaultHeaders(ctx context.Context, headers map[string]string) map[string]string {
	return auxApplyUserDefaultHeaders(ctx, headers)
}

// BuildORHeaders returns the OpenRouter headers plus response cache headers, env over config over default, nil config read from disk
func BuildORHeaders(ctx context.Context, orConfig map[string]any) (map[string]string, error) {
	headers := auxORHeadersBase()
	if orConfig == nil {
		orConfig = map[string]any{}
		if tree, err := auxLoadConfigReadonlyFn(ctx); err == nil {
			if section, ok := map[string]any(tree)["openrouter"].(map[string]any); ok {
				orConfig = section
			}
		}
	}
	envCache := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_OPENROUTER_CACHE")))
	enabled := pyrepr.Truthy(orConfig["response_cache"])
	if envCache != "" {
		enabled = envCache == "1" || envCache == "true" || envCache == "yes" || envCache == "on"
	}
	if !enabled {
		return headers, nil
	}
	headers["X-OpenRouter-Cache"] = "true"
	if envTTL := strings.TrimSpace(os.Getenv("ICEBATH_OPENROUTER_CACHE_TTL")); envTTL != "" {
		if auxIsDigitString(envTTL) {
			n, ok := auxPyIntDigits(envTTL)
			if !ok {
				return nil, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(envTTL))
			}
			if n >= 1 && n <= 86400 {
				headers["X-OpenRouter-Cache-TTL"] = strconv.Itoa(n)
			}
		}
		return headers, nil
	}
	ttl, has := orConfig["response_cache_ttl"]
	if !has {
		ttl = 300
	}
	var value float64
	switch v := ttl.(type) {
	case bool:
		value = 0
		if v {
			value = 1
		}
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	case float64:
		value = v
	default:
		return headers, nil
	}
	if value >= 1 && value <= 86400 {
		headers["X-OpenRouter-Cache-TTL"] = strconv.Itoa(int(math.Trunc(value)))
	}
	return headers, nil
}

// Python str.isdigit over a whole non empty string
func auxIsDigitString(s string) bool {
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// Python int value of a digit string capped past the TTL range, false for a non decimal digit
func auxPyIntDigits(s string) (int, bool) {
	n := 0
	for _, r := range s {
		d, ok := pystr.DecimalValue(r)
		if !ok {
			return 0, false
		}
		if n <= 86400 {
			n = n*10 + d
		}
	}
	return n, true
}

// BuildNvidiaNIMHeaders returns the NIM cloud attribution headers for integrate.api.nvidia.com traffic
func BuildNvidiaNIMHeaders(baseURL string) map[string]string {
	if !baseURLHostMatches(baseURL, "integrate.api.nvidia.com") {
		return map[string]string{}
	}
	out := map[string]string{}
	for k, v := range auxNvidiaNIMCloudHeaders {
		out[k] = v
	}
	return out
}

// Vercel AI Gateway attribution headers
func auxAIGatewayHeaders() map[string]string {
	return map[string]string{"HTTP-Referer": attributionReferer, "X-Title": attributionTitle, "User-Agent": userAgent("IcebathAgent")}
}

// AIGatewayHeaders is the AI Gateway attribution header set for callers outside the package
func AIGatewayHeaders() map[string]string { return auxAIGatewayHeaders() }

// Active profile's auth.json at call time so a multiplexed secondary never reads the launch profile's token
func auxAuthJSONPath(ctx context.Context) string {
	if auxAuthJSONPathOverride != "" {
		return auxAuthJSONPathOverride
	}
	return filepath.Join(home.Dir(ctx), "auth.json")
}

// True when the URL host is a MiniMax family host serving both API surfaces
func auxIsDualSurfaceAnthropicHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, suffix := range auxDualSurfaceAnthropicHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	for _, prefix := range auxDualSurfaceAnthropicHostPrefixes {
		if strings.HasPrefix(host, prefix) {
			return true
		}
	}
	return false
}

// OpenAI compatible sibling of a dual surface Anthropic URL, including the ZAI coding plan and Kimi Code
func auxToOpenAIBaseURL(baseURL string) string {
	raw := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURLHostname(raw) == "api.actual.inc" {
		return NormalizeActualBaseURL(raw)
	}
	if strings.HasSuffix(raw, "/anthropic") {
		stem := strings.TrimSuffix(raw, "/anthropic")
		if baseURLHostMatches(raw, "open.bigmodel.cn") || baseURLHostMatches(raw, "api.z.ai") {
			rewritten := stem + "/coding/paas/v4"
			slog.Debug("Auxiliary client: rewrote ZAI base URL " + raw + " → " + rewritten)
			return rewritten
		}
		if auxIsDualSurfaceAnthropicHost(raw) {
			rewritten := stem + "/v1"
			slog.Debug("Auxiliary client: rewrote dual-surface base URL " + raw + " → " + rewritten)
			return rewritten
		}
		slog.Debug("Auxiliary client: keeping Anthropic-only base URL " + raw + " (no dual-surface host match)")
		return raw
	}
	if baseURLHostMatches(raw, "api.kimi.com") && strings.HasSuffix(raw, "/coding") {
		rewritten := raw + "/v1"
		slog.Debug("Auxiliary client: rewrote Kimi base URL " + raw + " → " + rewritten)
		return rewritten
	}
	return raw
}

// ToOpenAIBaseURL is the OpenAI compatible sibling of a dual surface Anthropic URL
func ToOpenAIBaseURL(baseURL string) string { return auxToOpenAIBaseURL(baseURL) }
