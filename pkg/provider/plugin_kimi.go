package provider

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type modelsFetcher func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool)

// Profiles the bundled kimi-coding plugin registers, the global and the China endpoints
func kimiPluginProfiles() []Profile {
	mk := func(name string, aliases, keys []string, base string) Profile {
		p := bundledRowProfile(profileRow{name: name, keys: keys, base: base, omitTemp: true, maxTokens: 32000}, aliases...)
		p.LazyHeaders = NewLazyHeaders(HeaderEntry{Key: "HTTP-Referer", Value: attributionReferer}, HeaderEntry{Key: "X-Title", Value: attributionTitle},
			HeaderEntry{Key: "User-Agent", Fn: func() string { return UserAgent("IcebathAgent") }}, HeaderEntry{Key: "Accept-Encoding", Value: "gzip"})
		p.DefaultAuxModel = "kimi-k2-turbo-preview"
		plain := p
		p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
			return kimiFetchModels(ctx, plain.FetchModels, plain.BaseURL, apiKey, baseURL, timeout)
		}
		p.BuildAPIKwargsExtrasHook = kimiBuildAPIKwargsExtras
		return p
	}
	return []Profile{
		mk("kimi-coding", []string{"kimi", "moonshot", "kimi-for-coding"}, []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, "https://api.moonshot.ai/v1"),
		mk("kimi-coding-cn", []string{"kimi-cn", "moonshot-cn"}, []string{"KIMI_CN_API_KEY"}, "https://api.moonshot.cn/v1"),
	}
}

// True only for the canonical HTTPS Kimi Code API surfaces
func isConfirmedKimiCodingURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 || n != 443 {
			return false
		}
	}
	path := strings.TrimRight(u.Path, "/")
	return strings.ToLower(u.Scheme) == "https" && strings.ToLower(u.Hostname()) == "api.kimi.com" && u.User == nil &&
		(path == "/coding" || path == "/coding/v1") && u.RawQuery == "" && u.Fragment == ""
}

// Kimi Code models from its OpenAI compatible surface, the bare k3 slug dropped on every other endpoint
func kimiFetchModels(ctx context.Context, fetch modelsFetcher, profileBase, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
	effective := baseURL
	if effective == "" {
		effective = profileBase
	}
	effective = strings.TrimRight(effective, "/")
	confirmed := isConfirmedKimiCodingURL(effective)
	if confirmed {
		if u, err := url.Parse(effective); err == nil && strings.TrimRight(u.Path, "/") == "/coding" {
			effective += "/v1"
		}
	}
	models, ok := fetch(ctx, apiKey, effective, timeout)
	if !ok || confirmed {
		return models, ok
	}
	kept := []string{}
	for _, m := range models {
		if strings.ToLower(strings.TrimSpace(m)) != "k3" {
			kept = append(kept, m)
		}
	}
	return kept, true
}

// Effort when one is requested, else the thinking toggle, K2 thinking keeping replayed reasoning
func kimiBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	extraBody, topLevel := thinkingToggleExtras(hookReasoningCfg(reasoning), kimiK3Efforts, kimiK3Overrides, false)
	if thinking, ok := extraBody["thinking"].(map[string]any); ok && thinking["type"] == "enabled" &&
		strings.Contains(strings.ToLower(hookString(kw, "model")), "k2") {
		next := copyDict(thinking)
		next["keep"] = "all"
		extraBody["thinking"] = next
	}
	return extraBody, topLevel
}
