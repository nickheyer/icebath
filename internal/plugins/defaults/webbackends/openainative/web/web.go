package web

import (
	"context"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
)

// Provider kind whose sign in the Codex Responses transport runs on
const codex = "openai-codex"

// Answer of a client side search, reached only when the transport could not hand web_search to OpenAI
const unsupported = "openai-native declares OpenAI's server-side web_search tool; it cannot run as a client-side search and requires the " +
	"Codex Responses transport (provider openai-codex). For client-side search pick firecrawl or another backend with /web."

type backends struct {
	providers *provider.Registry
	creds     *credentials.Registry
}

func Register(registry *web.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(backends{providers, creds})
}

func (b backends) Backends() []web.Backend {
	return []web.Backend{{
		Name:  "openai-native",
		Title: "OpenAI Native Web Search (Codex Responses)",
		About: "OpenAI native web search — declares the Responses API server-side web_search built-in instead of running a client-side search. " +
			"Requires the Codex Responses transport plus an openai-codex sign in (ChatGPT or Codex Subscription, connected with /providers).",
		Plans: []web.Plan{{
			Title:  "OpenAI Native Web Search (Codex Responses)",
			Badge:  "native",
			About:  "Search runs on the provider side (needs the Codex Responses transport + an openai-codex login); search only, extraction still uses another backend",
			Prompt: "Uses the ChatGPT or Codex Subscription sign in: connect it with /providers",
		}},
		Search: func(context.Context, *config.Config, string, int) (*web.Results, error) {
			return web.Failed(unsupported), nil
		},
		Ready: func(cfg *config.Config) bool {
			_, ok := web.Credential(b.providers, b.creds, cfg, codex)
			return ok
		},
	}}
}
