package providers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const anthropicModelsMaxPages = 20

type anthropicProfile struct {
	base
	d Deps
}

func newAnthropic(d Deps) Profile {
	return &anthropicProfile{d: d, base: base{spec: Spec{
		Name: "anthropic", Aliases: []string{"claude", "claude-oauth", "claude-code"}, Mode: wire.ModeAnthropic,
		EnvVars: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"},
		BaseURL: "https://api.anthropic.com", AuthType: authTypeAPIKey, DefaultAuxModel: "claude-haiku-4-5-20251001",
	}}}
}

func anthropicModelsURL(baseURL, afterID string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	u := endpoint + "/v1/models"
	if strings.HasSuffix(endpoint, "/v1") {
		u = endpoint + "/models"
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	u += sep + "limit=1000"
	if afterID != "" {
		u += "&after_id=" + url.QueryEscape(afterID)
	}
	return u
}

func anthropicNextCursor(page wire.Value, seen map[string]bool) string {
	if !page.IsObject() || !page.Get("has_more").True() {
		return ""
	}
	last, ok := page.Get("last_id").Str()
	if !ok || last == "" || seen[last] {
		return ""
	}
	seen[last] = true
	return last
}

func (p *anthropicProfile) ListModels(ctx context.Context, in ListInput) ([]string, bool) {
	key := in.APIKey.Reveal()
	if key == "" {
		return nil, false
	}
	models := []string{}
	seen := map[string]bool{}
	cursor := ""
	for range anthropicModelsMaxPages {
		header := http.Header{}
		header.Set("x-api-key", key)
		header.Set("anthropic-version", "2023-06-01")
		header.Set("Accept", "application/json")
		page, err := p.d.Net.getJSON(ctx, anthropicModelsURL(in.BaseURL, cursor), listTimeout(in), header)
		if err != nil {
			slog.Debug(fmt.Sprintf("fetch_models(anthropic): %v", err))
			return nil, false
		}
		for _, m := range page.Get("data").Elems() {
			if id := m.Get("id"); m.IsObject() && id.Exists() {
				if s := id.Display(); !slices.Contains(models, s) {
					models = append(models, s)
				}
			}
		}
		if cursor = anthropicNextCursor(page, seen); cursor == "" {
			break
		}
	}
	return models, true
}
