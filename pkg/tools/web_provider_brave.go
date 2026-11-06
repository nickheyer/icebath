package tools

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
)

// Test seam for the Brave free tier search endpoint
var braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

type braveFreeProvider struct{ baseWebProvider }

// Search the free tier Data for Search API, count clamped to Brave's 1 to 20 window
func (p *braveFreeProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	apiKey, err := providerEnv(ctx, env, "BRAVE_SEARCH_API_KEY")
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return searchFail("BRAVE_SEARCH_API_KEY is not set"), nil
	}
	data, failure := httpGetJSON(ctx, env, "Brave Search", braveEndpoint,
		pyhttpx.Params{{"q", query}, {"count", itoa(max(1, min(limit, 20)))}},
		[][2]string{{"X-Subscription-Token", apiKey}, {"Accept", "application/json"}}, 15*time.Second, "")
	if failure != nil {
		return failure, nil
	}
	web, err := pyGetDefault(data, "web", nil)
	if err != nil {
		return nil, err
	}
	results, err := pyGetDefault(pyOr(web, map[string]any{}), "results", []any{})
	if err != nil {
		return nil, err
	}
	raw := pyOr(results, []any{})
	head, err := pySeqHead(raw, limit)
	if err != nil {
		return nil, err
	}
	rows, err := pyIter(head)
	if err != nil {
		return nil, err
	}
	hits, err := titledRows(rows, "description")
	if err != nil {
		return nil, err
	}
	all, _ := pyIter(raw)
	total := len(all)
	envLog(env).Info("Brave Search results", "query", query, "results", len(hits), "raw", total, "limit", limit)
	return searchOK(hits), nil
}

// SetupSchema is the icebath tools picker entry
func (p *braveFreeProvider) SetupSchema() map[string]any {
	return setupSchema("Brave Search (Free)", "free", "Free-tier API key — 2k queries/mo, search only.",
		"BRAVE_SEARCH_API_KEY", "Brave Search API key (free tier)", "https://brave.com/search/api/", nil)
}

func init() {
	RegisterWebProvider(&braveFreeProvider{baseWebProvider{name: "brave-free", display: "Brave Search (Free)", keyEnv: "BRAVE_SEARCH_API_KEY"}})
}
