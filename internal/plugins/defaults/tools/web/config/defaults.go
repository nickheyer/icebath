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
		Title:  "Web tools",
		About:  "Limits and caching of web searches and page reads",
		Order:  40,
		Settings: []config.Setting{
			{Key: CacheEnabled, Title: "Reuse recent results", About: "Reuse recent search results and pages instead of fetching them again.", Default: true},
			{Key: CacheExemptHosts, Title: "Always fresh sites", About: "Sites whose pages are always fetched fresh.", Help: "A site covers its subdomains as well.",
				Default: []string{}},
			{Key: ExtractCharLimit, Title: "Page text kept", About: "The most text that is kept from one web page.",
				Help: "A longer page keeps its start and end, and the whole text is saved to a file.", Default: 15000, Counts: "characters",
				Least: config.Num(MinCharLimit), Most: config.Num(MaxCharLimit), Advanced: true},
			{Key: ExtractTimeout, Title: "Page read time limit", About: "How long one page read may take before it is given up.", Default: 2 * time.Minute,
				Zero: "No limit", Advanced: true},
			{Key: CacheTTLMinutes, Title: "Cache time", About: "How long a cached result is reused.", Default: 20 * time.Minute, Unit: time.Minute,
				Min: config.Bound(time.Minute), Max: config.Bound(24 * time.Hour), Advanced: true},
			{Key: Debug, Title: "Debug log", About: "Record every web search and page read in a JSON file in the logs folder.", Default: false,
				Env: []string{"WEB_TOOLS_DEBUG"}, Advanced: true},
		},
	}})
}
