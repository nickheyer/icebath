package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	opencodeGoUsageURL     = "https://opencode.ai/zen/go/v1/usage"
	opencodeGoModelMaxToks = map[string]int{"mimo-v2.5-pro": 131072}
	opencodeThinkingIDs    = []string{"deepseek-reasoner", "deepseek-flash"}
)

// Profiles the bundled opencode-zen plugin registers, Zen then Go
func opencodePluginProfiles() []Profile {
	zen := bundledRowProfile(profileRow{name: "opencode-zen", keys: []string{"OPENCODE_ZEN_API_KEY"}, base: "https://opencode.ai/zen/v1"},
		"opencode", "opencode_zen", "zen")
	zen.LazyHeaders = attributionLazyHeaders("IcebathAgent")
	zen.DefaultAuxModel = "gemini-3-flash"
	zen.BuildAPIKwargsExtrasHook = func(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
		return oxAlphaReasoningExtras(hookReasoningCfg(reasoning), hookString(kw, "model"))
	}
	goProfile := bundledRowProfile(profileRow{name: "opencode-go", keys: []string{"OPENCODE_GO_API_KEY"}, base: "https://opencode.ai/zen/go/v1",
		noVisionTool: true}, "opencode_go", "go", "opencode-go-sub")
	goProfile.LazyHeaders = attributionLazyHeaders("IcebathAgent")
	goProfile.DefaultAuxModel = "glm-5"
	def := goProfile.DefaultMaxTokens
	goProfile.GetMaxTokensHook = func(model string) int {
		if limit, ok := opencodeGoModelMaxToks[flatModelName(model)]; ok {
			return limit
		}
		return def
	}
	goProfile.FetchAccountUsageHook = func(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
		return opencodeGoFetchAccountUsage(ctx, "opencode-go", baseURL, apiKey)
	}
	goProfile.BuildAPIKwargsExtrasHook = opencodeGoBuildAPIKwargsExtras
	return []Profile{zen, goProfile}
}

// Ox Alpha reasoning effort for the free preview model, low high or max only
func oxAlphaReasoningExtras(c reasoningCfg, model string) (map[string]any, map[string]any) {
	if flatModelName(model) != "x-preview-f-free" {
		return map[string]any{}, map[string]any{}
	}
	effort := requestedEffort(c)
	if effort == "none" {
		effort = ""
	}
	if clamped := clampEffort(effort, oxAlphaEfforts, oxAlphaOverrides); effort != "" && containsStr(oxAlphaEfforts, clamped) {
		return map[string]any{}, map[string]any{"reasoning_effort": clamped}
	}
	return map[string]any{}, map[string]any{}
}

func opencodeIsDeepseekThinkingModel(model string) bool {
	m := flatModelName(model)
	return strings.HasPrefix(m, "deepseek-v") && !strings.HasPrefix(m, "deepseek-v3") || containsStr(opencodeThinkingIDs, m)
}

// GLM 5.2 effort knob, Kimi K2 thinking toggle, and DeepSeek thinking toggle per model family
func opencodeGoBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	model := hookString(kw, "model")
	c := hookReasoningCfg(reasoning)
	switch {
	case containsAnySub(flatModelName(model), "glm-5.2", "glm-5-2", "glm-5p2"):
		effort := requestedEffort(c)
		if effort == "" || effort == "none" {
			return map[string]any{}, map[string]any{}
		}
		clamped := clampEffort(effort, glm52Efforts, glm52Overrides)
		if !containsStr(glm52Efforts, clamped) {
			clamped = "high"
		}
		return map[string]any{}, map[string]any{"reasoning_effort": clamped}
	case strings.HasPrefix(flatModelName(model), "kimi-k2"):
		if reasoning == nil {
			return map[string]any{}, map[string]any{}
		}
		return thinkingToggleExtras(c, kimiK2Efforts, nil, false)
	case opencodeIsDeepseekThinkingModel(model):
		return thinkingToggleExtras(c, deepseekV4Efforts, deepseekV4Overrides, false)
	}
	return map[string]any{}, map[string]any{}
}

// Go subscription windows from the literal usage endpoint, nil when the runtime has no key
func opencodeGoFetchAccountUsage(ctx context.Context, provider, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	runtime, err := modelsResolver(ctx).ResolveRuntime(provider, apiKey, baseURL, nil, "")
	if err != nil {
		return nil, err
	}
	token := ""
	if runtime != nil {
		token = strings.TrimSpace(runtime.APIKey)
	}
	if token == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opencodeGoUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		kind := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error", 5: "Server error"}[resp.StatusCode/100]
		return nil, fmt.Errorf("%s '%d %s' for url '%s'", kind, resp.StatusCode, http.StatusText(resp.StatusCode), opencodeGoUsageURL)
	}
	var payload any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	doc, err := pyDictOrEmpty(payload)
	if err != nil {
		return nil, err
	}
	usage, err := pyDictOrEmpty(doc["usage"])
	if err != nil {
		return nil, err
	}
	var windows []AccountUsageWindow
	for _, kl := range [][2]string{{"rolling", "Rolling window"}, {"weekly", "Weekly"}, {"monthly", "Monthly"}} {
		window, err := pyDictOrEmpty(usage[kl[0]])
		if err != nil {
			return nil, err
		}
		if window["percent"] == nil {
			continue
		}
		var resetAt *time.Time
		if pyTruthy(window["resetsAt"]) {
			raw := strings.ReplaceAll(pyScalarStr(window["resetsAt"]), "Z", "+00:00")
			t, ok := parseISO(raw)
			if !ok {
				return nil, fmt.Errorf("Invalid isoformat string: %s", pyRepr(raw))
			}
			resetAt = &t
		}
		pct, err := catalogFloat(window["percent"])
		if err != nil {
			return nil, err
		}
		windows = append(windows, AccountUsageWindow{Label: kl[1], UsedPercent: &pct, ResetAt: resetAt, Scope: "account"})
	}
	return &AccountUsageSnapshot{Provider: provider, Source: "go_usage_api", FetchedAt: time.Now().UTC(), Title: "Account limits", Windows: windows}, nil
}

// Dict of a falsy or dict value, the attribute error Python raises calling get on anything else
func pyDictOrEmpty(v any) (map[string]any, error) {
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	if !pyTruthy(v) {
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v))
}
