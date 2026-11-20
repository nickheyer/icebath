package remote

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const botProtocolHeading = "## Messaging other agents"

func init() {
	config.RegisterConfigFacade(config.FacadeHooks{Migration: testMigrationSources})
}

// Profile homes the ladder walks, the default home first and then every named profile under it
func testBotRoster(h string) []config.BotProfile {
	root := home.Root()
	roster := []config.BotProfile{{Name: "default", Dir: root}}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, e := range entries {
		if e.IsDir() {
			roster = append(roster, config.BotProfile{Name: e.Name(), Dir: filepath.Join(root, "profiles", e.Name())})
		}
	}
	return roster
}

// Ladder collaborators the provider registers in a real process, so in memory migrations run every step here too
func testMigrationSources(h string) config.MigrationSources {
	return config.MigrationSources{
		BotProtocolHeading: botProtocolHeading,
		BotRoster:          testBotRoster,
		StripLegacyBotProtocol: func(text string) string {
			before, _, _ := strings.Cut(text, botProtocolHeading)
			return before
		},
		ParseConfigStringList: config.AsStrings,
		ExplicitToolsetKeys:   func() map[string]bool { return map[string]bool{} },
		ToolsetAllowedForPlatform: func(string, string) bool {
			return true
		},
		ParseEnabledFlag: func(v any, def bool) bool {
			if v == nil {
				return def
			}
			return config.PyTruthy(v)
		},
		LegacyRelayPluginKeys: func(any) []string { return nil },
		CustomProviderEntryToProviderConfig: func(entry any, key string) map[string]any {
			m, _ := entry.(map[string]any)
			return map[string]any{"name": m["name"], "api": m["base_url"]}
		},
		RecordCoreUse: func(string, map[string]any, int) bool { return false },
		Browser: config.BrowserEnvSources{
			HomeDotenv:         config.LoadEnv,
			IsLaunchHome:       func(h string) bool { return home.Key(h) == home.Key(home.Root()) },
			PMStoreRoots:       func() ([]string, error) { return nil, nil },
			DeferStartupNotice: func(string) {},
		},
	}
}
