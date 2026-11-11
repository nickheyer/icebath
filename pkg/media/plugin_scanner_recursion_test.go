package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Home discovered by a fresh plugin manager with the image gen registry installed as the provider registry
func scannerImageHome(t *testing.T, setup func(h string)) (*plugins.Manager, context.Context) {
	t.Helper()
	resetImageRegistry(t)
	env := imageEnv(t, config.Tree{})
	t.Setenv("ICEBATH_HOME", env.Home)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	plugins.InstallServices(&plugins.Services{ProviderRegistries: map[string]plugins.ScopedRegistry{"image_gen_provider": ImageGenScopedRegistry()}})
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	restoreInterpreter := plugins.SetHostInterpreterForTests(python)
	savedSecrets := tools.ProfileSecrets
	tools.ProfileSecrets = provider.ProfileSecretSource()
	plugins.ResetPluginManagersForTests()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		restoreInterpreter()
		plugins.InstallServices(nil)
		tools.ProfileSecrets = savedSecrets
	})
	setup(env.Home)
	ctx := envContext(context.Background(), env)
	m := plugins.GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, false)
	return m, ctx
}

// Listing row of one plugin key, nil when the manager never recorded it
func scannerPluginRow(m *plugins.Manager, key string) map[string]any {
	for _, row := range m.ListPlugins() {
		if row["key"] == key {
			return row
		}
	}
	return nil
}

func TestBundledBackendAutoLoadBundledImageGenOpenaiAutoloads(t *testing.T) {
	m, _ := scannerImageHome(t, func(string) {})
	loaded := scannerPluginRow(m, "image_gen/openai")
	if loaded == nil {
		t.Fatalf("image_gen/openai not recorded: %v", m.ListPlugins())
	}
	if loaded["source"] != "bundled" || loaded["kind"] != "backend" || loaded["enabled"] != true {
		t.Fatalf("loaded %v", loaded)
	}
}

func TestRegisterImageGenProviderAcceptsValidProvider(t *testing.T) {
	m, ctx := scannerImageHome(t, func(h string) {
		dir := filepath.Join(h, "plugins", "my-img-plugin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"plugin.yaml": "name: my-img-plugin\nversion: 0.1.0\ndescription: Test plugin my-img-plugin\n",
			"__init__.py": "def register(ctx):\n    from agent.image_gen_provider import ImageGenProvider\n    class P(ImageGenProvider):\n        @property\n" +
				"        def name(self): return 'fake-ctx'\n        def generate(self, prompt, aspect_ratio='landscape', **kw):\n" +
				"            return {'success': True, 'image': 'x://y'}\n    ctx.register_image_gen_provider(P())\n",
			filepath.Join("..", "..", "config.yaml"): "plugins:\n  enabled: [my-img-plugin]\n",
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	loaded := scannerPluginRow(m, "my-img-plugin")
	if loaded == nil || loaded["enabled"] != true {
		t.Fatalf("loaded %v", loaded)
	}
	if GetImageGenProvider(ctx, "fake-ctx", "") == nil {
		t.Fatal("fake-ctx is not in the image gen registry")
	}
}
