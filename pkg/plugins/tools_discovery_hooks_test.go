package plugins

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsPluginDiscoveryHookRunsDiscovery(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if tools.EnsurePluginsDiscovered == nil {
		t.Fatal("discovery hook not installed")
	}
	ctx := home.WithOverride(context.Background(), h)
	if err := tools.EnsurePluginsDiscovered(ctx); err != nil {
		t.Fatal(err)
	}
	m := GetPluginManager(ctx)
	m.mu.RLock()
	discovered := m.discovered
	m.mu.RUnlock()
	if !discovered {
		t.Fatal("manager not discovered after the hook ran")
	}
}
