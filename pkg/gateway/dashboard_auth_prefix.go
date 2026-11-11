package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const dashboardMaxPrefixLength = 256

const dashboardRejectChars = "\"'<> \n\r\t"

var (
	dashboardPrefixWarnMu        sync.Mutex
	dashboardWarnedPublicURLs    = map[[2]string]bool{}
	dashboardWarnedPrefixes      = map[[2]string]bool{}
	dashboardLoadDashboardConfig = loadDashboardSection
)

func dashboardPrefixLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_cli.dashboard_auth.prefix")
}

// Logs the warning once per distinct key and only for a non-empty cleaned value
func dashboardWarnOnce(seen map[[2]string]bool, key [2]string, cleaned, msg string) {
	if cleaned == "" {
		return
	}
	dashboardPrefixWarnMu.Lock()
	if seen[key] {
		dashboardPrefixWarnMu.Unlock()
		return
	}
	seen[key] = true
	dashboardPrefixWarnMu.Unlock()
	dashboardPrefixLog().Warn(msg)
}

// Clears the remembered malformed prefix and public URL warnings so each warns afresh
func ResetDashboardPrefixWarnings() {
	dashboardPrefixWarnMu.Lock()
	defer dashboardPrefixWarnMu.Unlock()
	clear(dashboardWarnedPublicURLs)
	clear(dashboardWarnedPrefixes)
}

// Warns once when a non-empty public URL value from source was rejected
func dashboardWarnIfMalformed(source, raw string) {
	cleaned := pystr.Strip(raw)
	parts := strings.Split(cleaned, "://")
	example := parts[len(parts)-1]
	if example == "" {
		example = "hermes.example.com"
	}
	dashboardWarnOnce(dashboardWarnedPublicURLs, [2]string{source, cleaned}, cleaned, fmt.Sprintf(
		"%s is set to %s but was ignored because it is not a valid absolute URL — it must include an http:// or https:// scheme (e.g. https://%s). Falling back to reconstructing the OAuth redirect URI from request headers, which may produce the wrong scheme behind a reverse proxy.",
		source, pyrepr.Repr(cleaned), example))
}

// Warns once when a non-empty X-Forwarded-Prefix value is rejected for reason
func dashboardWarnIfMalformedPrefix(raw, reason string) {
	cleaned := pystr.Strip(raw)
	dashboardWarnOnce(dashboardWarnedPrefixes, [2]string{cleaned, reason}, cleaned, fmt.Sprintf(
		"X-Forwarded-Prefix header %s was ignored because %s. Dashboard URLs will be generated without a reverse-proxy path prefix.",
		pyrepr.Repr(cleaned), reason))
}

// Slash led prefix without a trailing slash, empty when unset or carrying traversal or injection characters
func NormalisePrefix(raw string) string {
	p := pystr.Strip(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if strings.Contains(p, "//") || strings.Contains(p, "..") || strings.ContainsAny(p, dashboardRejectChars) {
		dashboardWarnIfMalformedPrefix(raw, "it contains a disallowed character or path sequence")
		return ""
	}
	if pystr.Len(p) > dashboardMaxPrefixLength {
		dashboardWarnIfMalformedPrefix(raw, fmt.Sprintf("it is longer than %d characters", dashboardMaxPrefixLength))
		return ""
	}
	return p
}

// Cleaned scheme and netloc URL without a trailing slash, empty when blank, malformed or injection suspect
func normalisePublicURL(raw string) string {
	u := pystr.Strip(raw)
	if u == "" || strings.ContainsAny(u, dashboardRejectChars) {
		return ""
	}
	parsed, err := pyurl.Parse(u)
	if err != nil {
		return ""
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Netloc == "" {
		return ""
	}
	return strings.TrimRight(u, "/")
}

// Dashboard block of config.yaml, empty when unloadable, absent or not a mapping
func loadDashboardSection(ctx context.Context) map[string]any {
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		dashboardPrefixLog().Debug(fmt.Sprintf("dashboard-auth.prefix: load_config() raised %s; falling back to env-only configuration", err))
		return map[string]any{}
	}
	section, ok := cfg["dashboard"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return section
}

// Operator declared public URL from the listener, the env var, then dashboard.public_url, empty to reconstruct
func ResolvePublicURL(ctx context.Context, state *ListenerState) string {
	if state != nil && state.ListenerPublicURL != nil {
		return *state.ListenerPublicURL
	}
	envRaw := os.Getenv("ICEBATH_DASHBOARD_PUBLIC_URL")
	if envClean := normalisePublicURL(envRaw); envClean != "" {
		return envClean
	}
	dashboardWarnIfMalformed("ICEBATH_DASHBOARD_PUBLIC_URL env var", envRaw)
	cfgRaw := ""
	if v, ok := dashboardLoadDashboardConfig(ctx)["public_url"]; ok {
		cfgRaw = pyrepr.Str(v)
	}
	cfgClean := normalisePublicURL(cfgRaw)
	if cfgClean == "" {
		dashboardWarnIfMalformed("dashboard.public_url in config.yaml", cfgRaw)
	}
	return cfgClean
}
