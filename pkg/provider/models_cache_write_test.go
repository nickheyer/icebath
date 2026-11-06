package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestAtomicCacheWriteAllowsLiveProfileHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	def := filepath.Join(tmp, ".icebath")
	worker := filepath.Join(def, "profiles", "worker")
	if err := os.MkdirAll(worker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worker, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	cachePath := filepath.Join(worker, "cache", "models.json")
	if err := modelsWriteJSONCache(cachePath, map[string]any{"cached": true}, pyjson.Indent(2)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cachePath); string(b) != "{\n  \"cached\": true\n}" {
		t.Fatalf("got %q", b)
	}
}

func TestAtomicCacheWriteAllowsUnrelatedProfilesPath(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "custom", "profiles", "cache", "models.json")
	if err := modelsWriteJSONCache(cachePath, map[string]any{"cached": true}, pyjson.Indent(2)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cachePath); string(b) != "{\n  \"cached\": true\n}" {
		t.Fatalf("got %q", b)
	}
}
