package provider

import (
	"net/url"
	"slices"
	"strings"
)

// Window Kimi's coding endpoint gives Kimi K3, its million tokens, on the canonical https address only
func contextLength(model, base string) int {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Port() != "" && parsed.Port() != "443" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.ToLower(parsed.Hostname()) == "api.kimi.com" && (path == "/coding" || path == "/coding/v1") &&
		slices.Contains([]string{"k3", "kimi-k3", "kimi-k3-cot"}, strings.ToLower(strings.TrimSpace(model))) {
		return 1_048_576
	}
	return 0
}
