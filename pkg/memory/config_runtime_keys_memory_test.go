package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSelectedMemoryProviderDeclaresItsConfigKeys(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv(plugins.EnvEnableProjectPlugins, "")
	os.Unsetenv(plugins.EnvEnableProjectPlugins)
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.MemoryProviderDir = ProviderDirForHome
	plugins.InstallServices(&next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
	var out bytes.Buffer
	prevOut := config.FacadeStdout
	config.FacadeStdout = &out
	t.Cleanup(func() { config.FacadeStdout = prevOut; config.ResetCache() })
	dir := filepath.Join(h, "plugins", "widgetmem")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"name": "widgetmem", "config_keys": []string{"memory.widgetmem"}})
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	initSource := "from agent.memory_provider import MemoryProvider\n\nclass WidgetMem(MemoryProvider):\n    pass\n"
	if err := os.WriteFile(filepath.Join(dir, "__init__.py"), []byte(initSource), 0o644); err != nil {
		t.Fatal(err)
	}
	src := config.ConfigCommandSources{
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			SelectionConfigKeys: tools.SelectionConfigKeys,
			ActiveManifests:     func() []config.PluginConfigKeys { return plugins.ActiveConfigKeyManifests(h) },
		},
		LeftCoreTerminalKeys: provider.LeftCoreTerminalKeys,
		IsEnvSettingKey:      provider.IsEnvSettingKey,
		SaveEnvSetting:       provider.SaveEnvSetting,
		ReadEnvSetting:       provider.ReadEnvSetting,
		RemoveEnvSetting:     provider.RemoveEnvSetting,
		DropStaleModelRoute:  provider.DropStaleModelRoute,
	}
	notice := func(value string) bool {
		out.Reset()
		config.ResetCache()
		if err := config.SetConfigValue(context.Background(), h, "memory.widgetmem.project", value, false, src); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(out.String(), "not a recognized config key")
	}
	writeConfig := func(data map[string]any) {
		text, _ := json.Marshal(data)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), text, 0o600); err != nil {
			t.Fatal(err)
		}
		config.ResetCache()
	}
	if !notice("p1") {
		t.Fatal("installed but not selected must notice")
	}
	writeConfig(map[string]any{"memory": map[string]any{"provider": "widgetmem"}})
	if notice("p2") {
		t.Fatal("selected memory provider key noticed")
	}
	writeConfig(map[string]any{"memory": map[string]any{"provider": "widgetmem"}, "plugins": map[string]any{"disabled": []any{"widgetmem"}}})
	if !notice("p3") {
		t.Fatal("selected but disabled must notice")
	}
}
