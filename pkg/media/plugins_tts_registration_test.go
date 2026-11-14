package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Compiled-in plugin whose register body is the one a test supplies
type ttsProbePlugin struct {
	name     string
	register func(ctx *plugins.Context) error
}

func (p ttsProbePlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: p.name, Version: "0.1.0", Description: "Test plugin " + p.name}
}

func (p ttsProbePlugin) Register(ctx *plugins.Context) error { return p.register(ctx) }

// Enables one compiled-in plugin in a fresh home, discovers it and returns the home context with its list row
func loadTTSPlugin(t *testing.T, name string, register func(ctx *plugins.Context) error) (context.Context, map[string]any) {
	t.Helper()
	resetTTSRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n  - "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plugins.SwapBundled(ttsProbePlugin{name: name, register: register}))
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.ProviderRegistries = map[string]plugins.ScopedRegistry{"tts_provider": TTSScopedRegistry()}
	plugins.InstallServices(&next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
	ctx := home.WithOverride(context.Background(), h)
	m := plugins.NewManager(h)
	m.DiscoverAndLoad(ctx, false)
	t.Cleanup(func() { m.Unload(nil) })
	for _, row := range m.ListPlugins() {
		if row["name"] == name {
			return ctx, row
		}
	}
	t.Fatalf("plugin %s was not discovered", name)
	return nil, nil
}

func TestRegisterTTSProviderAcceptsValidProvider(t *testing.T) {
	ctx, row := loadTTSPlugin(t, "my-tts-plugin", func(c *plugins.Context) error {
		_, err := c.RegisterTTSProvider(newFakeTTSProvider("fake-tts"))
		return err
	})
	if row["enabled"] != true {
		t.Fatalf("Plugin failed to load: %v", row["error"])
	}
	if GetTTSProvider(ctx, "fake-tts", "") == nil {
		t.Fatal("the plugin's TTS provider is not in the registry")
	}
}

func TestRegisterTTSProviderRejectsNonProvider(t *testing.T) {
	ctx, row := loadTTSPlugin(t, "bad-tts-plugin", func(c *plugins.Context) error {
		_, err := c.RegisterTTSProvider("not a provider")
		return err
	})
	if row["enabled"] != true {
		t.Fatalf("Plugin failed to load: %v", row["error"])
	}
	if GetTTSProvider(ctx, "not a provider", "") != nil || len(ListTTSProviders(ctx, "")) != 0 {
		t.Fatalf("a non provider reached the registry: %v", ListTTSProviders(ctx, ""))
	}
}

func TestRegisterTTSProviderRejectsBuiltinShadow(t *testing.T) {
	ctx, row := loadTTSPlugin(t, "shadow-tts-plugin", func(c *plugins.Context) error {
		_, err := c.RegisterTTSProvider(newFakeTTSProvider("edge"))
		return err
	})
	if row["enabled"] != true {
		t.Fatalf("Plugin failed to load: %v", row["error"])
	}
	if GetTTSProvider(ctx, "edge", "") != nil {
		t.Fatal("a plugin shadowed the built-in edge provider")
	}
}
