package monitoring

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Mapping view of a config value, false for anything else
func asConfigMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, m != nil
	case config.Tree:
		return m, m != nil
	}
	return nil, false
}

// Install ids minted where the profile could not store one, keyed by home
var ephemeralInstallIDs sync.Map

// Stable install id of the profile, a fresh uuid minted and saved when empty
func EnsureInstallID(ctx context.Context) string {
	if existing := strings.TrimSpace(settings.MonitoringInstallId.Get(ctx)); existing != "" {
		return existing
	}
	h := home.Dir(ctx)
	if id, ok := ephemeralInstallIDs.Load(h); ok {
		return id.(string)
	}
	minted := uuid.NewString()
	if _, err := settings.Set(ctx, settings.MonitoringInstallId.Key, minted, settings.Global); err != nil {
		pkgLog().Debug(fmt.Sprintf("install_id persist failed; using an id for this process: %v", err))
		id, _ := ephemeralInstallIDs.LoadOrStore(h, minted)
		return id.(string)
	}
	return minted
}
