package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Facade home beside a managed scope directory, with a writer that drops every cache after each file
func managedHomes(t *testing.T) (string, func(scope, body string)) {
	t.Helper()
	h, _ := facadeHome(t, "")
	managed := filepath.Join(t.TempDir(), "managed")
	if err := os.Mkdir(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvManagedDir, managed)
	InvalidateManagedCache()
	ResetCache()
	t.Cleanup(InvalidateManagedCache)
	dirs := map[string]string{"home": h, "managed": managed}
	return h, func(scope, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dirs[scope], "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		ResetCache()
		InvalidateManagedCache()
	}
}

func TestManagedBeatsUser(t *testing.T) {
	h, write := managedHomes(t)
	write("home", "model:\n  default: user/model\n")
	write("managed", "model:\n  default: managed/model\n")
	if got := moduleLoad(t, h).Get("model.default"); got != "managed/model" {
		t.Fatalf("model.default %v", got)
	}
}

func TestManagedListWinsWholesale(t *testing.T) {
	h, write := managedHomes(t)
	write("home", "toolsets:\n  enabled: [a, b, c]\n")
	write("managed", "toolsets:\n  enabled: [x]\n")
	if got := moduleLoad(t, h).Get("toolsets.enabled"); !reflect.DeepEqual(got, []any{"x"}) {
		t.Fatalf("toolsets.enabled %v", got)
	}
}

func TestUserCannotShadowManagedLiteralViaEnvref(t *testing.T) {
	h, write := managedHomes(t)
	t.Setenv("EVIL", "user/override")
	write("home", "model:\n  default: ${EVIL}\n")
	write("managed", "model:\n  default: managed/locked\n")
	if got := moduleLoad(t, h).Get("model.default"); got != "managed/locked" {
		t.Fatalf("model.default %v", got)
	}
}

func TestManagedNestedDictDefaultFlattensOnLoad(t *testing.T) {
	h, write := managedHomes(t)
	write("home", "model:\n  default: user/model\n")
	write("managed", "model:\n  default:\n    provider: nous\n    model: managed/nested\n")
	cfg := moduleLoad(t, h)
	if cfg.Get("model.default") != "managed/nested" || cfg.Get("model.provider") != "nous" {
		t.Fatalf("model %v", cfg["model"])
	}
}

func TestManagedBareStringModelFlattensToDefaultOnLoad(t *testing.T) {
	h, write := managedHomes(t)
	write("home", "model:\n  default: user/model\n")
	write("managed", "model: managed/bare\n")
	if got := moduleLoad(t, h).Get("model.default"); got != "managed/bare" {
		t.Fatalf("model.default %v", got)
	}
}

func TestUserConfigOverridesDefault(t *testing.T) {
	h, _, _ := moduleHome(t, "\nmodel:\n  default: user/model-x\n")
	if got := moduleLoad(t, h).Get("model.default"); got != "user/model-x" {
		t.Fatalf("model.default %v", got)
	}
}

func TestEnvExpansionInUserConfig(t *testing.T) {
	t.Setenv("MY_BASE", "https://example.test")
	h, _, _ := moduleHome(t, "\nproviders:\n  custom:\n    base_url: ${MY_BASE}/v1\n")
	if got := moduleLoad(t, h).Get("providers.custom.base_url"); got != "https://example.test/v1" {
		t.Fatalf("base_url %v", got)
	}
}
