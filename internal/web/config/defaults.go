package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the web settings, the backend choices offered from the backends serving the capability, every backend for none
func Register(cfg *config.Config, backends func(capability string) []config.Option) {
	cfg.Register(config.Sections{
		{
			Key:   Key,
			Title: "Web",
			About: "Web search and page reading: which service answers, free plans, and the addresses web tools may open",
			Settings: []config.Setting{
				{Key: Backend, Title: "Web search", About: "Which service answers the agent's web searches and reads pages. Automatic picks the first service with a key, else a free one", Default: "", Options: func() []config.Option { return backends("") }},
				{Key: SearchBackend, Title: "Web search provider", About: "Provider used for web search. Empty follows the main web provider", Default: "", Options: func() []config.Option { return backends("search") }},
				{Key: ExtractBackend, Title: "Page reader provider", About: "Provider used to read web pages. Empty follows the main web provider", Default: "", Options: func() []config.Option { return backends("extract") }},
				{Key: FreeFallback, Title: "Free web providers", About: "Use free providers when no web search key is set", Default: true},
				{Key: FreeRescue, Title: "Free provider retry", About: "Retry with a free provider when the chosen one fails. Needs free web providers to be on", Default: true},
				{Key: ProviderTier, Title: "Provider plan", About: "Per provider: use its free plan (free), a paid key (paid), or decide automatically (auto)", Default: map[string]any{}},
				{Key: PrivateURLs, Title: "Allow private URLs", About: "Let web tools open local and private network addresses", Default: false},
				{Key: HostedParsing, Title: "Hosted PDF reading", About: "Read scanned PDF pages through a web service that parses documents, Firecrawl when its key is set", Default: true},
				{Key: FakeIPRanges, Title: "Fake IP ranges", About: "Address ranges a local DNS proxy hands out for real sites. Web tools check the real address instead of these", Default: []string{}},
			},
		},
	})
	cfg.Register(config.Sections{
		{
			Key:    Blocklist,
			Parent: Key,
			Title:  "Website blocklist",
			About:  "Sites web tools refuse to open",
			Settings: []config.Setting{
				{Key: BlocklistEnabled, Title: "Website blocklist", About: "Block web tools from the sites on the list", Default: false},
				{Key: BlocklistDomains, Title: "Blocked sites", About: "Websites web tools refuse to open. A site blocks its subdomains, *.site only its subdomains", Default: []string{}},
				{Key: BlocklistSharedFiles, Title: "Blocklist files", About: "Files of blocked sites to load as well, one site a line. A relative path is under the profile's folder", Default: []string{}},
			},
		},
	})
}
