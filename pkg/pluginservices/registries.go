package pluginservices

import (
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Registry of every provider kind a plugin registers into, the one map the process wiring and the test binaries install
func ProviderRegistries() map[string]plugins.ScopedRegistry {
	return map[string]plugins.ScopedRegistry{
		"image_gen_provider":            media.ImageGenScopedRegistry(),
		"video_gen_provider":            media.VideoGenScopedRegistry(),
		"tts_provider":                  media.TTSScopedRegistry(),
		"transcription_provider":        media.TranscriptionScopedRegistry(),
		"web_search_provider":           agent.WebSearchScopedRegistry(),
		"browser_provider":              browser.BrowserProviderRegistry.Scoped(),
		"browser_page_backend":          browser.BrowserPageBackendScopedRegistry(),
		"terminal_environment_provider": agent.TerminalEnvScopedRegistry(),
		"secret_source":                 provider.SecretSourceScopedRegistry(),
		"delegation_provider":           agent.DelegationScopedRegistry(),
		"login_backend":                 browser.LoginBackendScopedRegistry(),
	}
}
