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
		Title: "OpenAI web search",
		About: "Search that OpenAI runs, with your ChatGPT or Codex sign-in",
		Plans: []web.Plan{{
			Title:  "With your sign-in",
			Badge:  "built in · search only",
			About:  "Needs a ChatGPT or Codex sign-in, and another service to read pages",
			Prompt: "ChatGPT or Codex sign-in",
			SignIn: codex,
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
