package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	if err := modelsWriteJSONCache(context.Background(), cachePath, map[string]any{"cached": true}, pyjson.Indent(2)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cachePath); string(b) != "{\n  \"cached\": true\n}" {
		t.Fatalf("got %q", b)
	}
}

func TestAtomicCacheWriteAllowsUnrelatedProfilesPath(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "custom", "profiles", "cache", "models.json")
	if err := modelsWriteJSONCache(context.Background(), cachePath, map[string]any{"cached": true}, pyjson.Indent(2)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cachePath); string(b) != "{\n  \"cached\": true\n}" {
		t.Fatalf("got %q", b)
	}
}

func TestLateReasoningCapsSaveDoesNotRecreateDeletedHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	def := filepath.Join(tmp, ".icebath")
	worker := filepath.Join(def, "profiles", "worker")
	if err := os.MkdirAll(filepath.Dir(worker), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, worker)
	(&capsCatalog{url: "https://example.test/v1/models"}).saveDisk(context.Background(), map[string]reasoningCaps{"example/model": {known: true, supports: true}})
	if _, err := os.Stat(worker); !os.IsNotExist(err) {
		t.Fatalf("a late save recreated the deleted profile home: %v", err)
	}
}

func TestReasoningCapsSaveWritesCompactJSONThroughTheCacheWriter(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(reasoningCapsDiskPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reasoningCapsDiskPath(), []byte(`{"z":{"ts":1,"caps":{}},"a":{"ts":2,"caps":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	(&capsCatalog{url: "https://example.test/v1/models"}).saveDisk(context.Background(), map[string]reasoningCaps{"example/model": {known: true, supports: true, supportedEfforts: []string{"low", "high"}}})
	b, err := os.ReadFile(reasoningCapsDiskPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	prefix := "{\n\"z\":{\n\"ts\":1,\n\"caps\":{}\n},\n\"a\":{\n\"ts\":2,\n\"caps\":{}\n},\n\"https://example.test/v1/models\":{\n\"ts\":"
	suffix := ",\n\"caps\":{\n\"example/model\":{\n\"known\":true,\n\"supports_reasoning\":true,\n\"supported_efforts\":[\n\"low\",\n\"high\"\n],\n\"mandatory\":false\n}\n}\n}\n}"
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) {
		t.Fatalf("got %q", got)
	}
	cached, _ := (&capsCatalog{url: "https://example.test/v1/models"}).loadDisk()
	if cp, ok := cached["example/model"]; !ok || !cp.supports || len(cp.supportedEfforts) != 2 {
		t.Fatalf("round trip %+v", cached)
	}
}
