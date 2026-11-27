package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Web",
		About:  "Web search and page reading: page text kept, read timeout, and the cache of recent results",
		Settings: []config.Setting{
			{Key: ExtractCharLimit, Title: "Page text limit", About: "Most characters kept from one web page, between 2000 and 500000. A longer page shows its head and tail, the whole text saved to a file", Default: 15000},
			{Key: ExtractTimeout, Title: "Page read timeout", About: "Seconds one page read may take before it is given up. Zero waits without a limit", Default: 2 * time.Minute},
			{Key: CacheEnabled, Title: "Web cache", About: "Reuse recent web search and page results", Default: true},
			{Key: CacheExemptHosts, Title: "Uncached sites", About: "Sites whose pages are always fetched fresh. Matches a site and its subdomains", Default: []string{}},
			{Key: CacheTTLMinutes, Title: "Web cache time", About: "Minutes a cached web result is reused, between 1 and 1440", Default: 20 * time.Minute,
				Unit: time.Minute, Min: config.Bound(time.Minute), Max: config.Bound(24 * time.Hour)},
			{Key: Debug, Title: "Web tools debug log", About: "Record every web_search and web_extract call to a JSON file in the profile's logs folder", Default: false, Env: []string{"WEB_TOOLS_DEBUG"}},
		},
	}})
}
