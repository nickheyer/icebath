package memory

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

type unavailableSetupProvider struct{ regProvider }

func (*unavailableSetupProvider) Available(context.Context) bool { return false }

func (*unavailableSetupProvider) ConfigSchema() []map[string]any {
	return []map[string]any{{"key": "api_key", "env_var": "HINDSIGHT_API_KEY", "secret": true, "url": "https://ui.hindsight.vectorize.io"}}
}

func TestSetupWithProviderArgSkipsPicker(t *testing.T) {
	m, _ := setupFor(t)
	var direct []string
	var saved map[string]any
	m.availableProviders = func(context.Context) []setupProvider {
		return []setupProvider{{"honcho", "requires API key", &regProvider{name: "honcho"}}}
	}
	m.cursesSelect = func(context.Context, string, [][2]string, int, int) (int, error) {
		t.Fatal("picker opened for a named provider")
		return setupCancelled, nil
	}
	m.installDependencies = func(_ context.Context, name string) error { direct = append(direct, name); return nil }
	m.loadConfig = func(context.Context) (map[string]any, error) { return map[string]any{"memory": map[string]any{}}, nil }
	m.saveConfig = func(_ context.Context, cfg map[string]any) error { saved = cfg; return nil }
	if rc, err := m.Run(context.Background(), "setup", "honcho"); rc != 0 || err != nil {
		t.Fatal(rc, err)
	}
	if !reflect.DeepEqual(direct, []string{"honcho"}) || saved["memory"].(map[string]any)["provider"] != "honcho" {
		t.Fatal(direct, saved)
	}
}

func TestUnknownProviderReportsAndReturnsEarly(t *testing.T) {
	m, out := setupFor(t)
	m.loadConfig = func(context.Context) (map[string]any, error) {
		t.Fatal("config loaded for an unknown provider")
		return nil, nil
	}
	m.saveConfig = func(context.Context, map[string]any) error {
		t.Fatal("config saved for an unknown provider")
		return nil
	}
	if err := m.CmdSetupProvider(context.Background(), "notaprovider"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "not found") || !strings.Contains(got, "icebath memory setup") {
		t.Fatal(got)
	}
}

func TestStatusSurfacesEnvInheritanceHintWhenUnavailable(t *testing.T) {
	m, out := setupFor(t)
	t.Setenv("HINDSIGHT_API_KEY", "")
	os.Unsetenv("HINDSIGHT_API_KEY")
	m.availableProviders = func(context.Context) []setupProvider {
		return []setupProvider{{"hindsight", "cloud", &unavailableSetupProvider{regProvider{name: "hindsight"}}}}
	}
	m.loadConfig = func(context.Context) (map[string]any, error) {
		return map[string]any{"memory": map[string]any{"provider": "hindsight", "hindsight": map[string]any{}}}, nil
	}
	if err := m.CmdStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "not available") || !strings.Contains(got, "HINDSIGHT_API_KEY") || !strings.Contains(got, ".env") {
		t.Fatal(got)
	}
}

func TestMemorySetupSendsCandidatePathsInsteadOfDiscoveryCallbacks(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "provider")
	discWrite(t, filepath.Join(candidate, "plugin.yaml"), "name: provider\npython_dependencies: [fixture-dep==1]\n")
	previousFind := setupFindProviderDir
	setupFindProviderDir = func(context.Context, *tools.Env, string) string { return candidate }
	t.Cleanup(func() { setupFindProviderDir = previousFind })
	_, inputs, err := MemoryProviderDependencyInputs(context.Background(), testEnv(t), "provider")
	if err != nil || inputs == nil || len(inputs.Extras) != 0 || !reflect.DeepEqual(inputs.Plugins, pm.PluginInput(&pm.Candidates{Dirs: []string{candidate}})) {
		t.Fatalf("inputs %+v %v", inputs, err)
	}
}
