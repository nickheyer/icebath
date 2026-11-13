package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	missingNoticeMu sync.Mutex
	missingNoticed  = map[[2]string]bool{}
)

// Install command for a catalog plugin of the category, empty when the catalog lacks it or the lookup fails
func CatalogInstallHint(ctx context.Context, name, category string) string {
	if name == "" {
		return ""
	}
	entry, err := plugins.GetPluginCatalogEntry(name)
	if err != nil || entry == nil || (category != "" && entry.Category != category) {
		return ""
	}
	return provider.PluginInstallCommand(name, home.Dir(ctx))
}

// Notice for a configured provider that resolves nowhere, once per process per home and name, true when a fresh scan finds it
func RecoverMemoryProviderAtStartup(ctx context.Context, name string, say func(string)) (bool, error) {
	if FindProvider(ctx, nil, name) != nil {
		return true, nil
	}
	h := home.Dir(ctx)
	key := [2]string{home.Key(h), name}
	missingNoticeMu.Lock()
	seen := missingNoticed[key]
	missingNoticed[key] = true
	missingNoticeMu.Unlock()
	if seen {
		return false, nil
	}
	message := fmt.Sprintf("⚠ Memory provider '%s' is not installed, so external memory is off for this session. "+
		"Install a runtime plugin that provides it under %s or change memory.provider.", name, home.PluginsDir(h))
	if hint := CatalogInstallHint(ctx, name, "memory"); hint != "" {
		message = fmt.Sprintf("⚠ Memory provider '%s' is not installed, so external memory is off for this session. "+
			"Install it with `%s` or change memory.provider.", name, hint)
	}
	slog.Default().Warn(message)
	if say != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Default().Debug("Memory provider notice failed", "panic", r)
				}
			}()
			say(message)
		}()
	}
	return false, nil
}
