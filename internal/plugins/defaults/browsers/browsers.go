package browsers

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browserbase"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/browseruse"
	"github.com/nickheyer/icebath/internal/plugins/defaults/browsers/firecrawl"
)

// Registers the cloud browser services, Browser Use first as the one keys alone pick before Browserbase
func Register(registries *plugins.Registries) {
	browseruse.Register(registries)
	browserbase.Register(registries)
	firecrawl.Register(registries)
}
