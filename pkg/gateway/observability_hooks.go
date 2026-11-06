package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
)

func init() {
	observability.ConnectedPlatformsHook = connectedPlatformCount
	observability.SetCatalogSource("bundled_platform_names", bundledPlatformNames)
}

// Number of messaging platforms the loaded gateway config connects
func connectedPlatformCount(ctx context.Context) (int, error) {
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		return 0, err
	}
	return len(cfg.GetConnectedPlatforms(ctx)), nil
}

// Swappable install root whose plugins/platforms tree lists the shipped messaging platforms
var bundledPlatformsRepoRoot = pm.RepoRoot

// Lowercased names of the shipped platform plugin directories that carry a plugin.yaml, empty when unreadable
func bundledPlatformNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(bundledPlatformsRepoRoot(), "plugins", "platforms"))
	if err != nil {
		return []string{}, nil
	}
	seen := map[string]bool{}
	names := []string{}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(bundledPlatformsRepoRoot(), "plugins", "platforms", e.Name(), "plugin.yaml"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if name := strings.ToLower(e.Name()); !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
