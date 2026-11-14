package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	prevHonored := ManagedConfigHonored
	ManagedConfigHonored = func() bool { return false }
	t.Cleanup(func() { ManagedConfigHonored = prevHonored })
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

// Managed dir under a temp root holding the given config and env bodies, cache dropped
func writeManagedScope(t *testing.T, config, env *string) string {
	t.Helper()
	md := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	if config != nil {
		if err := os.WriteFile(filepath.Join(md, "config.yaml"), []byte(*config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if env != nil {
		if err := os.WriteFile(filepath.Join(md, ".env"), []byte(*env), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvManagedDir, md)
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	return md
}

func TestLoadManagedEnvAndIsEnvManaged(t *testing.T) {
	env := "OPENAI_API_BASE=https://org.example/v1\n"
	writeManagedScope(t, nil, &env)
	if got := LoadManagedEnv(); !reflect.DeepEqual(got, map[string]string{"OPENAI_API_BASE": "https://org.example/v1"}) {
		t.Fatal(got)
	}
	if !IsEnvManaged("OPENAI_API_BASE") || IsEnvManaged("OTHER") {
		t.Fatal("is env managed")
	}
}

func TestManagedEnvStripsTheBOMAndRefusesInvalidUTF8Loudly(t *testing.T) {
	env := "\xef\xbb\xbfOPENAI_API_BASE=https://org.example/v1\n"
	md := writeManagedScope(t, nil, &env)
	if got := LoadManagedEnv(); !reflect.DeepEqual(got, map[string]string{"OPENAI_API_BASE": "https://org.example/v1"}) {
		t.Fatalf("%q", got)
	}
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := os.WriteFile(filepath.Join(md, ".env"), []byte("KEY=caf\xe9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadManagedEnv(); len(got) != 0 {
		t.Fatalf("invalid utf-8 must be ignored, got %q", got)
	}
	if !strings.Contains(logged.String(), "IGNORING") || !strings.Contains(logged.String(), "can't decode byte 0xe9 in position 7") {
		t.Fatal(logged.String())
	}
}

func TestManagedConfigNonMappingIsEmptyWithoutWarning(t *testing.T) {
	cfg := "- a\n- b\n"
	writeManagedScope(t, &cfg, nil)
	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if got := LoadManagedConfig(); len(got) != 0 || logged.Len() != 0 {
		t.Fatalf("%v %s", got, logged.String())
	}
}

func TestManagedCacheFollowsFileEdits(t *testing.T) {
	env := "A=1\n"
	md := writeManagedScope(t, nil, &env)
	first := LoadManagedEnv()
	first["A"] = "mutated"
	if LoadManagedEnv()["A"] != "1" {
		t.Fatal("cached value must be a copy")
	}
	if err := os.WriteFile(filepath.Join(md, ".env"), []byte("A=22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if LoadManagedEnv()["A"] != "22" {
		t.Fatal("edit must invalidate the cached parse")
	}
}

func TestOverlayNoopWithoutScope(t *testing.T) {
	t.Setenv(EnvManagedDir, filepath.Join(t.TempDir(), "nope"))
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	src := map[string]any{"display": map[string]any{"skin": "user"}}
	if out := ApplyManagedOverlay(src); !reflect.DeepEqual(out, map[string]any{"display": map[string]any{"skin": "user"}}) {
		t.Fatal(out)
	}
}

func TestOverlayPreservesUserSiblings(t *testing.T) {
	cfg := "display:\n  skin: charizard\n"
	writeManagedScope(t, &cfg, nil)
	out := ApplyManagedOverlay(map[string]any{"display": map[string]any{"skin": "user", "show_reasoning": true}})
	display := out["display"].(map[string]any)
	if display["skin"] != "charizard" || display["show_reasoning"] != true {
		t.Fatal(out)
	}
}

func TestGatewayEnvBridgeHonorsManaged(t *testing.T) {
	cfg := "timezone: Asia/Tokyo\n"
	writeManagedScope(t, &cfg, nil)
	raw, err := FastSafeLoad([]byte("timezone: America/New_York\n"))
	if err != nil {
		t.Fatal(err)
	}
	if bridged := ApplyManagedOverlay(AsMap(raw)); bridged["timezone"] != "Asia/Tokyo" {
		t.Fatal(bridged)
	}
}

func TestPinnedTerminalBackendIgnoresEmptyOrNull(t *testing.T) {
	for _, body := range []string{"terminal:\n  backend:\n", "terminal:\n  backend: '   '\n", "terminal: docker\n"} {
		writeManagedScope(t, &body, nil)
		if got := PinnedTerminalBackend(); got != "" {
			t.Fatalf("%q pinned %q", body, got)
		}
	}
	body := "terminal:\n  backend: ' ssh '\n"
	writeManagedScope(t, &body, nil)
	if got := PinnedTerminalBackend(); got != "ssh" {
		t.Fatal(got)
	}
}
