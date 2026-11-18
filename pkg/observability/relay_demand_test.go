package observability

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestEnabledSharedMetricsMakeTheProfileARelayConsumer(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(tools.RelayPluginsConfigEnv, "")
	for _, name := range tools.LegacyRelayExportEnvVars {
		t.Setenv(name, "")
	}
	resetForTests()
	t.Cleanup(resetForTests)
	ctx := context.Background()
	files := len(tools.RelayDiscoveredPluginFiles()) > 0
	if got := tools.RelayDemanded(ctx); got != files {
		t.Fatalf("demand %v without shared metrics, plugin files %v", got, files)
	}
	writeProfileConfig(t, h, enabledConfig)
	if !tools.RelayDemanded(ctx) {
		t.Fatal("enabled shared metrics do not demand Relay")
	}
}
