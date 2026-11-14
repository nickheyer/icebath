package providers

import (
	"context"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const KimiCodeBaseURL = "https://api.kimi.com/coding"

type kimiProfile struct {
	base
	d Deps
}

func newKimi(d Deps, name string) Profile {
	s := Spec{Name: name, OmitTemperature: true, DefaultMaxTokens: 32000, DefaultAuxModel: "kimi-k2-turbo-preview",
		DefaultHeaders: map[string]string{"HTTP-Referer": "https://icebath.app", "X-Title": "icebath",
			"User-Agent": userAgent(d, "icebath"), "Accept-Encoding": "gzip"}}
	if name == "kimi-coding-cn" {
		s.Aliases, s.EnvVars, s.BaseURL = []string{"kimi-cn", "moonshot-cn"}, []string{"KIMI_CN_API_KEY"}, "https://api.moonshot.cn/v1"
	} else {
		s.Aliases, s.EnvVars, s.BaseURL = []string{"kimi", "moonshot", "kimi-for-coding"}, []string{"KIMI_API_KEY", "KIMI_CODING_API_KEY"}, "https://api.moonshot.ai/v1"
	}
	return &kimiProfile{d: d, base: base{spec: s}}
}

func isConfirmedKimiCodingURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	port := u.Port()
	path := strings.TrimRight(u.EscapedPath(), "/")
	return strings.ToLower(u.Scheme) == "https" && strings.ToLower(u.Hostname()) == "api.kimi.com" && (port == "" || port == "443") &&
		u.User == nil && (path == "/coding" || path == "/coding/v1") && u.RawQuery == "" && u.Fragment == ""
}

func (p *kimiProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	effective := in.BaseURL
	if effective == "" {
		effective = p.spec.BaseURL
	}
	effective = strings.TrimRight(effective, "/")
	confirmed := isConfirmedKimiCodingURL(effective)
	if u, err := url.Parse(effective); confirmed && err == nil && strings.TrimRight(u.EscapedPath(), "/") == "/coding" {
		effective += "/v1"
	}
	in.BaseURL = effective
	models, ok := FetchModels(ctx, p.d.Net, &p.spec, in)
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

func (p *kimiProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	body, top := wire.ThinkingToggleExtras(f.Reasoning, wire.EffortsKimiK3.Efforts(), false)
	if raw, ok := body["thinking"]; ok && strings.Contains(strings.ToLower(f.Model), "k2") {
		if v, err := wire.ParseValue(raw); err == nil && v.IsObject() && v.Get("type").Text() == "enabled" {
			body["thinking"] = wire.DictOf(v).SetString("keep", "all").JSON()
		}
	}
	return Extras{Body: body, TopLevel: top}, nil
}

func (p *kimiProfile) RuntimeBaseURL(_ context.Context, apiKey auth.Secret, def, envURL string) (string, bool) {
	if envURL != "" {
		return envURL, true
	}
	if strings.HasPrefix(apiKey.Reveal(), "sk-kimi-") {
		return KimiCodeBaseURL, true
	}
	return def, true
}
