package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Malware check",
		About: "Checks packages for known malware before they run",
		Group: config.GroupSafety,
		Order: 20,
		Settings: []config.Setting{
			{Key: Endpoint, Title: "Service address", About: "The address of the OSV service that is asked whether a package is known malware.",
				Default: "https://api.osv.dev/v1/query", Help: "Packages that MCP servers start with npx, uvx or pipx, and those that skills install, are checked.",
				Env: []string{"OSV_ENDPOINT"}, Advanced: true},
			{Key: CacheTTL, Title: "Keep answers for", About: "How long an answer of the malware check is kept.", Default: time.Hour,
				Env: []string{"OSV_CHECK_CACHE_TTL"}, Advanced: true},
		},
	}})
}
