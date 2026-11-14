package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const OpenCodeSessionHeader = "x-opencode-session"

var (
	openCodeFamilies    = []string{"opencode-go", "opencode-zen"}
	openCodeFamilyPaths = map[string]string{"opencode-zen": "/zen", "opencode-go": "/zen/go"}
	openCodeRelayPath   = regexp.MustCompile(`^/zen(/go)?(/v1)?$`)
	openCodeModes       = map[string][]struct {
		prefixes []string
		mode     wire.Mode
	}{
		"opencode-go": {{[]string{"gpt-", "grok-", "muse-spark"}, wire.ModeResponses},
			{[]string{"claude-", "minimax-", "qwen", "union-alpha"}, wire.ModeAnthropic}},
		"opencode-zen": {{[]string{"claude-", "union-alpha"}, wire.ModeAnthropic},
			{[]string{"gpt-", "grok-", "muse-spark"}, wire.ModeResponses}, {[]string{"qwen"}, wire.ModeAnthropic}},
	}
	azureFoundryResponsesPrefixes = []string{"codex", "gpt-5", "o1", "o3", "o4"}
	openCodeThinkingIDs           = []string{"deepseek-reasoner", "deepseek-flash"}
)

func AzureFoundryModelMode(model string) (wire.Mode, bool) {
	raw := flatModelName(model)
	if raw != "" && slices.ContainsFunc(azureFoundryResponsesPrefixes, func(p string) bool { return strings.HasPrefix(raw, p) }) {
		return wire.ModeResponses, true
	}
	return "", false
}

func (r *Registry) OpenCodeFamily(ctx context.Context, provider string) (string, bool) {
	raw := strings.ToLower(strings.TrimSpace(provider))
	if raw == "" {
		return "", false
	}
	if canonical := r.NormalizeProvider(ctx, provider); slices.Contains(openCodeFamilies, canonical) {
		return canonical, true
	}
	for _, f := range openCodeFamilies {
		if strings.HasPrefix(raw, f) {
			return f, true
		}
	}
	return "", false
}

func (r *Registry) NormalizeOpenCodeModel(ctx context.Context, provider, model string) string {
	family, ok := r.OpenCodeFamily(ctx, provider)
	current := strings.TrimSpace(model)
	if current == "" || !ok {
		return current
	}
	first := provider
	if first == "" {
		first = family
	}
	for _, prefix := range []string{first + "/", family + "/"} {
		if strings.HasPrefix(strings.ToLower(current), strings.ToLower(prefix)) {
			return current[len(prefix):]
		}
	}
	return current
}

func (r *Registry) OpenCodeModelMode(ctx context.Context, provider, model string) wire.Mode {
	family, _ := r.OpenCodeFamily(ctx, provider)
	normalized := strings.ToLower(r.NormalizeOpenCodeModel(ctx, provider, model))
	if normalized != "" {
		for _, rule := range openCodeModes[family] {
			if slices.ContainsFunc(rule.prefixes, func(p string) bool { return strings.HasPrefix(normalized, p) }) {
				return rule.mode
			}
		}
	}
	return wire.ModeChat
}

func (r *Registry) NormalizeOpenCodeBaseURL(ctx context.Context, provider string, mode wire.Mode, baseURL string) string {
	raw := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	family, ok := r.OpenCodeFamily(ctx, provider)
	if raw == "" || !ok {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := strings.ToLower(u.Hostname())
	official := host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
	path := strings.TrimRight(u.Path, "/")
	if official && slices.Contains(openCodeFamilies, r.NormalizeProvider(ctx, provider)) && openCodeRelayPath.MatchString(path) {
		suffix := ""
		if strings.HasSuffix(path, "/v1") {
			suffix = "/v1"
		}
		path = openCodeFamilyPaths[family] + suffix
	}
	if mode == wire.ModeAnthropic {
		path = strings.TrimSuffix(path, "/v1")
	} else if official && !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	u.Path, u.RawPath = path, ""
	return u.String()
}

func (r *Registry) IsOpenCodeTarget(ctx context.Context, provider, baseURL string) bool {
	if _, ok := r.OpenCodeFamily(ctx, provider); ok {
		return true
	}
	return HostMatches(baseURL, "opencode.ai")
}

func (r *Registry) OpenCodeSessionHeaders(ctx context.Context, provider, baseURL, sessionID string) map[string]string {
	if !r.IsOpenCodeTarget(ctx, provider, baseURL) {
		return nil
	}
	key := AffinityKey(ctx, sessionID)
	if key == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		key = "oneshot-" + hex.EncodeToString(b[:])
	}
	return map[string]string{OpenCodeSessionHeader: key}
}

func MergeAffinityHeaders(existing map[string]string, sets ...map[string]string) map[string]string {
	merged := map[string]string{}
	for k, v := range existing {
		merged[k] = v
	}
	for _, set := range sets {
		for k, v := range set {
			if _, ok := merged[k]; !ok {
				merged[k] = v
			}
		}
	}
	return merged
}

func CustomAffinityHeaders(ctx context.Context, header, sessionID string) map[string]string {
	if header == "" {
		return nil
	}
	if key := AffinityKey(ctx, sessionID); key != "" {
		return map[string]string{header: key}
	}
	return nil
}

type openCodeZenProfile struct {
	base
}

type openCodeGoProfile struct {
	base
	d Deps
}

const openCodeGoUsageURL = "https://opencode.ai/zen/go/v1/usage"

var openCodeResetLayouts = []string{"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05.999999999", "2006-01-02"}

func newOpenCodeZen(d Deps) Profile {
	return &openCodeZenProfile{base{spec: Spec{
		Name: "opencode-zen", Aliases: []string{"opencode", "opencode_zen", "zen"}, EnvVars: []string{"OPENCODE_ZEN_API_KEY"},
		BaseURL: "https://opencode.ai/zen/v1", DefaultHeaders: openCodeHeaders(d), DefaultAuxModel: "gemini-3-flash",
	}}}
}

func newOpenCodeGo(d Deps) Profile {
	return &openCodeGoProfile{d: d, base: base{spec: Spec{
		Name: "opencode-go", Aliases: []string{"opencode_go", "go", "opencode-go-sub"}, EnvVars: []string{"OPENCODE_GO_API_KEY"},
		BaseURL: "https://opencode.ai/zen/go/v1", DefaultHeaders: openCodeHeaders(d), DefaultAuxModel: "glm-5",
		RejectsVisionToolMessages: true,
	}}}
}

func openCodeHeaders(d Deps) map[string]string {
	return map[string]string{"HTTP-Referer": "https://icebath.app", "X-Title": "icebath", "User-Agent": userAgent(d, "icebath")}
}

func (p *openCodeZenProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	return Extras{TopLevel: wire.OxAlphaReasoning(f.Reasoning, f.Model)}, nil
}

func (p *openCodeGoProfile) MaxTokens(model string) (int, bool) {
	if flatModelName(model) == "mimo-v2.5-pro" {
		return 131072, true
	}
	return p.spec.DefaultMaxTokens, p.spec.DefaultMaxTokens > 0
}

func (p *openCodeGoProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	m := flatModelName(f.Model)
	switch {
	case strings.Contains(m, "glm-5.2") || strings.Contains(m, "glm-5-2") || strings.Contains(m, "glm-5p2"):
		effort := wire.RequestedEffort(f.Reasoning)
		if effort == "" || effort == "none" {
			return Extras{}, nil
		}
		family := wire.EffortsGLM52.Efforts()
		clamped := wire.ClampEffort(effort, family.Levels, family.Round)
		if !slices.Contains(family.Levels, clamped) {
			clamped = "high"
		}
		return effortTop(clamped), nil
	case strings.HasPrefix(m, "kimi-k2"):
		if f.Reasoning == nil {
			return Extras{}, nil
		}
		body, top := wire.ThinkingToggleExtras(f.Reasoning, wire.Efforts{Levels: wire.EffortsKimiK2.Efforts().Levels}, false)
		return Extras{Body: body, TopLevel: top}, nil
	case (strings.HasPrefix(m, "deepseek-v") && !strings.HasPrefix(m, "deepseek-v3")) || slices.Contains(openCodeThinkingIDs, m):
		body, top := wire.ThinkingToggleExtras(f.Reasoning, wire.EffortsDeepSeekV4.Efforts(), false)
		return Extras{Body: body, TopLevel: top}, nil
	}
	return Extras{}, nil
}

func (p *openCodeGoProfile) AccountUsage(ctx context.Context, baseURL string, apiKey auth.Secret) (*AccountUsage, error) {
	token := strings.TrimSpace(apiKey.Reveal())
	if token == "" {
		creds, err := p.d.Store.ResolveAPIKey(ctx, p.spec.Name)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(creds.APIKey.Reveal())
	}
	if token == "" {
		return nil, nil
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Accept", "application/json")
	data, err := p.d.Net.getJSON(ctx, openCodeGoUsageURL, 10*time.Second, header)
	if err != nil {
		return nil, fmt.Errorf("opencode go usage: %w", err)
	}
	usage := data.Get("usage")
	windows := []UsageWindow{}
	for _, w := range []struct{ key, label string }{{"rolling", "Rolling window"}, {"weekly", "Weekly"}, {"monthly", "Monthly"}} {
		win := usage.Get(w.key)
		pct, ok := win.Get("percent").Float()
		if !ok {
			continue
		}
		var reset time.Time
		if raw := strings.ReplaceAll(win.Get("resetsAt").Display(), "Z", "+00:00"); win.Get("resetsAt").Truthy() {
			var perr error
			for _, layout := range openCodeResetLayouts {
				if reset, perr = time.Parse(layout, raw); perr == nil {
					break
				}
			}
			if perr != nil {
				return nil, fmt.Errorf("opencode go usage: %w", perr)
			}
		}
		pctCopy := pct
		windows = append(windows, UsageWindow{Label: w.label, UsedPercent: &pctCopy, ResetAt: reset, Scope: "account"})
	}
	return &AccountUsage{Provider: p.spec.Name, Source: "go_usage_api", FetchedAt: time.Now().UTC(), Title: "Account limits", Windows: windows}, nil
}
