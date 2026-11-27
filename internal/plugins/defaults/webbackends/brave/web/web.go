package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	braveconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/brave/config"
	"github.com/nickheyer/icebath/internal/web"
)

const endpoint = "https://api.search.brave.com/res/v1/web/search"

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     "brave-free",
		Title:    "Brave Search",
		About:    "Web search on Brave's free plan, 2,000 searches a month",
		Settings: []string{braveconfig.APIKey},
		Plans: []web.Plan{{
			Title:  "Free plan",
			Badge:  "free · key needed · search only",
			About:  "A free API key for up to 2,000 searches a month",
			Key:    braveconfig.APIKey,
			Prompt: "Brave Search API key",
			Link:   "https://brave.com/search/api/",
		}},
		Search: search,
		Ready:  func(cfg *config.Config) bool { return key(cfg) != "" },
		Rank:   7,
	}}
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(braveconfig.APIKey))
}

// Searches the free tier, asking for between 1 and 20 results as Brave allows
func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	token := key(cfg)
	if token == "" {
		return web.Failed("BRAVE_SEARCH_API_KEY is not set"), nil
	}
	var answer struct {
		Web struct {
			Results []struct {
				Title       web.Text `json:"title"`
				URL         web.Text `json:"url"`
				Description web.Text `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	failure := web.GetJSON(ctx, "Brave Search", "", web.Request{
		URL:     endpoint,
		Query:   url.Values{"q": {query}, "count": {strconv.Itoa(max(1, min(limit, 20)))}},
		Header:  map[string]string{"X-Subscription-Token": token, "Accept": "application/json"},
		Timeout: 15 * time.Second,
	}, &answer)
	if failure != nil {
		return failure, nil
	}
	rows := answer.Web.Results[:min(max(limit, 0), len(answer.Web.Results))]
	hits := make([]web.Hit, len(rows))
	for i, row := range rows {
		hits[i] = web.Hit{Title: row.Title.String(), URL: row.URL.String(), Description: row.Description.String(), Position: i + 1}
	}
	return web.Found(hits), nil
}
