package webbackends

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/brave"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/ddgs"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/exa"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/firecrawl"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/keenable"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/openainative"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/parallel"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/perplexity"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/searxng"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/tavily"
	"github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/xai"
)

// Registers the web backends in the order a profile with several set up picks the first of, the free ring going round those of them
// with a free plan in the same order
func Register(registries *plugins.Registries) {
	tavily.Register(registries)
	perplexity.Register(registries)
	exa.Register(registries)
	parallel.Register(registries)
	keenable.Register(registries)
	firecrawl.Register(registries)
	searxng.Register(registries)
	brave.Register(registries)
	ddgs.Register(registries)
	xai.Register(registries)
	openainative.Register(registries)
}
