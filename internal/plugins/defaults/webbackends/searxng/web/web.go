package web

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	searxngconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/searxng/config"
	"github.com/nickheyer/icebath/internal/web"
)

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     "searxng",
		Title:    "SearXNG",
		About:    "SearXNG web search — free, self-hosted, privacy-respecting metasearch engine. Requires SEARXNG_URL pointing at your instance.",
		Settings: []string{searxngconfig.URL},
		Plans: []web.Plan{{
			Title:  "SearXNG",
			Badge:  "free · self-hosted",
			About:  "Free, privacy-respecting metasearch. Point SEARXNG_URL at your instance.",
			Key:    searxngconfig.URL,
			Prompt: "SearXNG instance URL (e.g. http://localhost:8080)",
			Link:   "https://searx.space/",
		}},
		Search: search,
		Ready:  func(cfg *config.Config) bool { return base(cfg) != "" },
		Rank:   6,
	}}
}

// Address of the instance without a trailing slash, empty when unset
func base(cfg *config.Config) string {
	return strings.TrimRight(strings.TrimSpace(cfg.GetString(searxngconfig.URL)), "/")
}

type result struct {
	Title   web.Text `json:"title"`
	URL     web.Text `json:"url"`
	Content web.Text `json:"content"`
	Score   float64  `json:"score"`
}

// Searches the instance, rows ordered by descending score
func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	address := base(cfg)
	if address == "" {
		return web.Failed("SEARXNG_URL is not set"), nil
	}
	var answer struct {
		Results []result `json:"results"`
	}
	failure := web.GetJSON(ctx, "SearXNG", "SearXNG at "+address, web.Request{
		URL:     address + "/search",
		Query:   url.Values{"q": {query}, "format": {"json"}, "pageno": {"1"}},
		Timeout: 15 * time.Second,
	}, &answer)
	if failure != nil {
		return failure, nil
	}
	rows := answer.Results
	slices.SortStableFunc(rows, func(a, b result) int { return cmp.Compare(b.Score, a.Score) })
	rows = rows[:min(max(limit, 0), len(rows))]
	hits := make([]web.Hit, len(rows))
	for i, row := range rows {
		hits[i] = web.Hit{Title: row.Title.String(), URL: row.URL.String(), Description: row.Content.String(), Position: i + 1}
	}
	return web.Found(hits), nil
}
