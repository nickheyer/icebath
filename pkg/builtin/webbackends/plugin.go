// Package webbackends compiles the bundled web search and extract backends in as plugins/web/<vendor> manager plugins
package webbackends

import (
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One bundled web backend plugin, its manifest fields from plugins/web/<dir>/plugin.yaml and its provider
type webBackendPlugin struct {
	dir, name, description string
	provider               func() tools.WebProvider
}

func (b webBackendPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: b.name, Version: "1.0.0", Description: b.description, Author: "Icebath", Kind: "backend",
		Source: "bundled", Key: "web/" + b.dir}
}

// Registers the backend provider into the web search registry of the profile being discovered
func (b webBackendPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterWebSearchProvider(b.provider())
	return err
}

// Bundled web backends in plugins tree order
var backends = []webBackendPlugin{
	{"brave_free", "web-brave-free", "Brave Search (free tier) — web search via Brave's Data-for-Search API. Requires BRAVE_SEARCH_API_KEY (free signup at https://brave.com/search/api/, 2k queries/month).",
		tools.NewBraveFreeWebProvider},
	{"ddgs", "web-ddgs", "DuckDuckGo web search through the ddgs client built into icebath — no API key required.",
		tools.NewDDGSWebProvider},
	{"exa", "web-exa", "Exa web search and content extraction. Requires EXA_API_KEY — sign up at https://exa.ai.", tools.NewExaWebProvider},
	{"firecrawl", "web-firecrawl", "Firecrawl web search + content extraction. Supports keyless cloud and the direct API.",
		tools.NewFirecrawlWebProvider},
	{"keenable", "web-keenable", "Keenable web search + page fetch (independent web index for AI apps). Works keyless on Keenable's free tier as part of the default rotation; set KEENABLE_API_KEY for higher limits — https://keenable.ai.",
		tools.NewKeenableWebProvider},
	{"openai_native", "web-openai-native", "OpenAI native web search — declares the Responses API server-side ``web_search`` built-in instead of running a client-side search. Requires the Codex Responses transport plus openai-codex OAuth (``icebath auth add openai-codex``).",
		tools.NewOpenAINativeWebProvider},
	{"parallel", "web-parallel", "Parallel.ai web search + content extraction. Search returns objective-tuned results; extract uses the async SDK for parallel page fetches. Requires PARALLEL_API_KEY — sign up at https://parallel.ai.",
		tools.NewParallelWebProvider},
	{"perplexity", "web-perplexity", "Perplexity Search API web search and query-relevant page snippets. Requires PERPLEXITY_API_KEY — get one at https://www.perplexity.ai/account/api.",
		tools.NewPerplexityWebProvider},
	{"searxng", "web-searxng", "SearXNG web search — free, self-hosted, privacy-respecting metasearch engine. Requires SEARXNG_URL pointing at your instance.",
		tools.NewSearXNGWebProvider},
	{"tavily", "web-tavily", "Tavily web search + extract. Opt-in keyless via icebath tools; set TAVILY_API_KEY for higher limits — https://app.tavily.com/home.",
		tools.NewTavilyWebProvider},
	{"xai", "web-xai", "xAI Web Search — search the web via Grok's agentic web_search tool (Responses API). Requires xAI Grok OAuth (via `icebath auth`) or XAI_API_KEY (https://x.ai).",
		tools.NewXAIWebProvider},
}

func init() {
	for _, b := range backends {
		plugins.RegisterBundled(b)
	}
}
