package provider

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Default auxiliary models and endpoints of the OpenRouter and Anthropic routes
const (
	auxOpenRouterModel         = "nvidia/nemotron-3-ultra-550b-a55b:free"
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

// OpenRouter attribution headers every OpenRouter call carries
func auxORHeadersBase() map[string]string {
	return pyhttpx.OrderedHeaderMap(pyhttpx.HeaderLine{Name: "HTTP-Referer", Value: attributionReferer},
		pyhttpx.HeaderLine{Name: "X-Title", Value: attributionTitle}, pyhttpx.HeaderLine{Name: "X-OpenRouter-Categories", Value: "productivity,cli-agent"})
}

// Merges model.default_headers over the resolved headers, user values winning
func auxApplyUserDefaultHeaders(ctx context.Context, headers map[string]string) map[string]string {
	user := settings.ModelDefaultHeaders.Get(ctx)
	if len(user) == 0 {
		return headers
	}
	var lines []pyhttpx.HeaderLine
	for _, k := range pyhttpx.HeaderOrder(headers) {
		lines = append(lines, pyhttpx.HeaderLine{Name: k, Value: headers[k]})
	}
	for _, k := range config.MappingKeys(user) {
		if v := user[k]; v != nil {
			lines = append(lines, pyhttpx.HeaderLine{Name: k, Value: pyrepr.Str(v)})
		}
	}
	if len(lines) == 0 {
		return headers
	}
	return pyhttpx.OrderedHeaderMap(lines...)
}

// Resolved headers with model.default_headers merged over them, user values winning
func ApplyUserDefaultHeaders(ctx context.Context, headers map[string]string) map[string]string {
	return auxApplyUserDefaultHeaders(ctx, headers)
}

// BuildORHeaders returns the OpenRouter headers plus the response cache headers the profile asks for
func BuildORHeaders(ctx context.Context) map[string]string {
	headers := auxORHeadersBase()
	if !settings.OpenrouterResponseCache.Get(ctx) {
		return headers
	}
	headers["X-OpenRouter-Cache"] = "true"
	pyhttpx.SetHeaderOrder(headers, append(pyhttpx.HeaderOrder(auxORHeadersBase()), "X-OpenRouter-Cache"))
	if ttl := settings.OpenrouterResponseCacheTtl.Get(ctx); ttl >= 1 && ttl <= 86400 {
		headers["X-OpenRouter-Cache-TTL"] = strconv.Itoa(ttl)
	}
	return headers
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
			pkgLog().Debug("Auxiliary client: rewrote ZAI base URL " + raw + " → " + rewritten)
			return rewritten
		}
		if auxIsDualSurfaceAnthropicHost(raw) {
			rewritten := stem + "/v1"
			pkgLog().Debug("Auxiliary client: rewrote dual-surface base URL " + raw + " → " + rewritten)
			return rewritten
		}
		pkgLog().Debug("Auxiliary client: keeping Anthropic-only base URL " + raw + " (no dual-surface host match)")
		return raw
	}
	if baseURLHostMatches(raw, "api.kimi.com") && strings.HasSuffix(raw, "/coding") {
		rewritten := raw + "/v1"
		pkgLog().Debug("Auxiliary client: rewrote Kimi base URL " + raw + " → " + rewritten)
		return rewritten
	}
	return raw
}

// ToOpenAIBaseURL is the OpenAI compatible sibling of a dual surface Anthropic URL
func ToOpenAIBaseURL(baseURL string) string { return auxToOpenAIBaseURL(baseURL) }
