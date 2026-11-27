package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Malware check",
		About: "Packages looked up in the OSV database before they run: those MCP servers launch with npx, uvx or pipx, and those skills install",
		Settings: []config.Setting{
			{Key: Endpoint, Title: "Malware check service", About: "OSV query endpoint asked whether a package is known malware",
				Default: "https://api.osv.dev/v1/query", Env: []string{"OSV_ENDPOINT"}},
			{Key: CacheTTL, Title: "Malware check cache", About: "Seconds an answer of the malware check is kept", Default: time.Hour,
				Env: []string{"OSV_CHECK_CACHE_TTL"}},
		},
	}})
}
