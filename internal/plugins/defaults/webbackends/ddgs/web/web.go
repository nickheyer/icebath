package web

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	ddgsconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/ddgs/config"
	"github.com/nickheyer/icebath/internal/web"
)

// Longest a whole search may take
const timeout = 30 * time.Second

type backends struct{}

func Register(registry *web.Registry) {
	registry.Register(backends{})
}

func (backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:     "ddgs",
		Title:    "DuckDuckGo (ddgs)",
		About:    "DuckDuckGo web search through the ddgs client built into icebath — no API key required.",
		Settings: []string{ddgsconfig.Region, ddgsconfig.SafeSearch, ddgsconfig.TimeLimit, ddgsconfig.Proxy},
		Plans: []web.Plan{{
			Title: "DuckDuckGo (ddgs)",
			Badge: "free · no key · search only",
			About: "Search via the built-in DuckDuckGo client — no API key (pair with any extract provider)",
		}},
		Search: search,
		// Off until a web backend setting names it
		Ready: func(cfg *config.Config) bool { return web.Picked(cfg, "ddgs") },
		Rank:  8,
	}}
}

// Searches the engines within the time a search may take, every failure a failed search
func search(ctx context.Context, cfg *config.Config, query string, limit int) (*web.Results, error) {
	capped, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hits, err := lookup(capped, cfg, query, max(1, limit))
	switch {
	case err == nil:
		return web.Found(hits), nil
	case ctx.Err() != nil:
		return web.Failed("DuckDuckGo search interrupted"), nil
	case capped.Err() != nil:
		return web.Failed(fmt.Sprintf("DuckDuckGo search timed out after %s — DuckDuckGo may be rate-limiting or slow. Try again later or switch to a different search provider.", timeout)), nil
	}
	return web.Failed("DuckDuckGo search failed: " + err.Error()), nil
}
