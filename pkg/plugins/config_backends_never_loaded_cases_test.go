package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// The general plugin manager never loads a config backend plugin
func TestGeneralPluginManagerNeverLoadsConfigBackends(t *testing.T) {
	p := remoteplane.Setup(t)
	t.Setenv(config.BackendEnv, "file")
	m := NewManager(p.Home)
	m.DiscoverAndLoad(context.Background(), false)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for key := range m.plugins {
		if strings.Contains(key, "config_backends") || key == "remote" {
			t.Fatalf("loaded %q", key)
		}
	}
}
