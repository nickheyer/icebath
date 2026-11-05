package provider

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

var actualGOOS = runtime.GOOS

// Profiles the bundled actual plugin registers
func actualPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "actual", display: "Actual Computer",
		desc:   "Actual Computer - hosted inference via api.actual.inc, or local offline inference via model.base_url in config.yaml",
		signup: "https://actual.inc", keys: []string{"ACTUAL_API_KEY"}, base: ActualBaseURL, authType: AuthTypeAPIKey, mode: ModeChatCompletions,
	}, "actual-computer", "actualcomputer", "aci")
	base := p
	p.BuildClientKwargsExtrasHook = func(kw map[string]any) map[string]any { return actualBuildClientKwargsExtras(base.BaseURL, kw) }
	p.SupportedReasoningEffortsHook = func(context.Context, string) ([]string, bool, error) {
		return append([]string(nil), actualRelayEfforts...), true, nil
	}
	p.BuildAPIKwargsExtrasHook = actualBuildAPIKwargsExtras
	p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
		if baseURL == "" {
			creds, err := modelsResolver(ctx).apiKeyProviderCredentials("actual")
			if err != nil {
				slog.Debug("fetch_models(actual): " + err.Error())
				return nil, false
			}
			baseURL = creds.BaseURL
		}
		return base.FetchModels(ctx, apiKey, normalizeActualRuntimeBaseURL(baseURL), timeout)
	}
	return []Profile{p}
}

// macOS clients of the hosted endpoint get the CA bundle unless a CA override variable is set
func actualBuildClientKwargsExtras(profileBase string, kw map[string]any) map[string]any {
	baseURL := pyScalarStrOr(kw["base_url"], profileBase)
	u, err := url.Parse(baseURL)
	if err != nil {
		return map[string]any{}
	}
	hostname := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if actualGOOS != "darwin" || hostname != "api.actual.inc" {
		return map[string]any{}
	}
	for _, key := range []string{"ICEBATH_CA_BUNDLE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if os.Getenv(key) != "" {
			return map[string]any{}
		}
	}
	if bundle := DefaultCABundlePath(); bundle != "" {
		return map[string]any{"ssl_ca_cert": bundle}
	}
	return map[string]any{}
}

// String of a truthy value, else the fallback
func pyScalarStrOr(v any, fallback string) string {
	if pyTruthy(v) {
		return pyScalarStr(v)
	}
	return fallback
}

// Relay thinking toggle with effort none as a real level that also turns thinking off
func actualBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	if reasoning == nil {
		return map[string]any{}, map[string]any{}
	}
	c := hookReasoningCfg(reasoning)
	if !c.disabled && c.effort == "none" {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}, map[string]any{"reasoning_effort": "none"}
	}
	return thinkingToggleExtras(c, actualRelayEfforts, nil, true)
}
