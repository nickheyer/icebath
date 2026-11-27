package config

import "github.com/nickheyer/icebath/internal/config"

// Registers the web settings, the backend choices offered from the backends serving the capability, every backend for none
func Register(cfg *config.Config, backends func(capability string) []config.Option) {
	cfg.Register(config.Sections{
		{
			Key:   Key,
			Title: "Web search",
			About: "Which service searches the web and reads pages",
			Group: config.GroupAbilities,
			Order: 30,
			Settings: []config.Setting{
				{Key: Backend, Title: "Service", About: "Which service searches the web and reads pages for the agent.", Default: "", Slash: "web",
					Options: func() []config.Option { return backends("") }},
				{Key: FreeFallback, Title: "Free services", About: "Use free services when you have no API key for web search.", Default: true},
				{Key: FreeRescue, Title: "Fall back to free", About: "Try a free service when the chosen service fails.",
					Help: "Needs Free services to be on.", Default: true},
				{Key: SearchBackend, Title: "Search service", About: "Which service answers searches, when it differs from the main one.", Default: "",
					Slash: "web", Options: func() []config.Option { return backends("search") }},
				{Key: ExtractBackend, Title: "Page reading service", About: "Which service reads pages, when it differs from the main one.", Default: "",
					Slash: "web", Options: func() []config.Option { return backends("extract") }},
				{Key: ProviderTier, Title: "Plans", About: "The plan that each service is used on.", Default: map[string]any{}, Slash: "web",
					Entry: &config.Entry{Noun: "service", Name: "Service name", Fields: []config.Setting{
						{Title: "Plan", About: "Which plan of the service is used.", Default: Auto, Options: config.Options(
							config.Option{Value: Auto, Title: "Automatic", About: "The free plan without a key, your own plan with one"},
							config.Option{Value: Free, Title: "Free", About: "The free plan, even when you have a key"},
							config.Option{Value: Paid, Title: "Paid", About: "Only with your API key"},
						)},
					}}},
				{Key: HostedParsing, Title: "Read scanned PDFs online", About: "Read scanned PDF pages through a web service, such as Firecrawl.",
					Help: "Scanned pages are read only by a service whose API key is set.", Default: true},
				{Key: PrivateURLs, Title: "Open private addresses", About: "Let web tools open local and private network addresses.", Default: false},
				{Key: FakeIPRanges, Title: "Fake IP ranges", About: "Address ranges that a local DNS proxy hands out for real sites.",
					Help: "Web tools check the real address instead of these.", Default: []string{}, Advanced: true},
			},
		},
	})
	cfg.Register(config.Sections{
		{
			Key:    Blocklist,
			Parent: Key,
			Title:  "Blocked sites",
			About:  "Sites that web tools refuse to open",
			Order:  110,
			Settings: []config.Setting{
				{Key: BlocklistEnabled, Title: "Turn on blocking", About: "Refuse to open the sites on the list.", Default: false},
				{Key: BlocklistDomains, Title: "Site list", About: "Sites that web tools refuse to open.",
					Help: "A site blocks its subdomains as well. Write *.site to block only the subdomains.", Default: []string{}},
				{Key: BlocklistSharedFiles, Title: "List files", About: "Files of more sites to block, one site per line.",
					Help: "A relative path is placed in the profile's folder.", Default: []string{}, Advanced: true},
			},
		},
	})
}
