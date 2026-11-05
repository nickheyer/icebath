package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedOverlayPinsValuesOverUserConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvManagedDir, dir)
	t.Setenv("PINNED_MODEL", "org/pinned")
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: ${PINNED_MODEL}\nterminal:\n  backend: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=managed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ManagedDir() != dir {
		t.Fatal("managed dir")
	}
	out := ApplyManagedOverlay(map[string]any{"model": map[string]any{"default": "user/model", "provider": "openai"}, "terminal": map[string]any{"timeout": 5}})
	model := out["model"].(map[string]any)
	if model["default"] != "org/pinned" || model["provider"] != "openai" {
		t.Fatalf("%v", out)
	}
	terminal := out["terminal"].(map[string]any)
	if terminal["backend"] != "docker" || terminal["timeout"] != 5 {
		t.Fatalf("%v", terminal)
	}
	if !IsKeyManaged("model") || IsKeyManaged("model.default") || IsKeyManaged("terminal.timeout") || !IsEnvManaged("OPENAI_API_KEY") || IsEnvManaged("OTHER") {
		t.Fatal("key queries")
	}
	if PinnedTerminalBackend() != "docker" {
		t.Fatal("pinned backend")
	}
	keys := ManagedConfigKeys()
	if len(keys) != 2 || keys[0] != "model" || keys[1] != "terminal.backend" {
		t.Fatal(keys)
	}
	ManagedConfigHonored = func() bool { return false }
	t.Cleanup(func() { ManagedConfigHonored = func() bool { return true } })
	if len(LoadManagedConfig()) != 0 {
		t.Fatal("backend that does not honor managed config")
	}
}

func TestManagedOverlayFailsOpenOnMalformedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvManagedDir, dir)
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("model: [unclosed\n"), 0o644)
	doc := map[string]any{"a": 1}
	if out := ApplyManagedOverlay(doc); out["a"] != 1 || len(out) != 1 {
		t.Fatal(out)
	}
	t.Setenv(EnvManagedDir, filepath.Join(dir, "missing"))
	if ManagedDir() != "" || len(LoadManagedEnv()) != 0 {
		t.Fatal("missing dir must mean no scope")
	}
}

func TestLoadUserEffectiveAppliesOverlayWithoutDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvManagedDir, dir)
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("onboarding:\n  run: false\n"), 0o644)
	user := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(user, []byte("display:\n  in_app_tips: ${TIPS}\n"), 0o644)
	lookup := func(name string) (string, bool) {
		if name == "TIPS" {
			return "false", true
		}
		return "", false
	}
	doc, err := LoadUserEffective(user, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if doc["onboarding"].(map[string]any)["run"] != false || doc["display"].(map[string]any)["in_app_tips"] != "false" {
		t.Fatal(doc)
	}
	if _, ok := doc["agent"]; ok {
		t.Fatal("defaults must not be merged")
	}
	if doc, err := LoadUserEffective(filepath.Join(t.TempDir(), "none.yaml"), lookup); err != nil || doc["onboarding"] == nil {
		t.Fatal("absent user file still gets the managed layer")
	}
}
