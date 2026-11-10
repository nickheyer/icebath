package profiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Profile bob with a config model, display meta and a distribution manifest, the memo cleared around the test
func listingProfile(t *testing.T) string {
	t.Helper()
	ResetFileCache()
	t.Cleanup(ResetFileCache)
	p := filepath.Join(t.TempDir(), "profiles", "bob")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"config.yaml":       "model:\n  provider: openai\n  default: gpt-4o\n",
		"profile.yaml":      "display_name: Bob\ndescription: first\n",
		"distribution.yaml": "name: starter\nversion: 1.0.0\nsource: registry\n",
	} {
		if err := os.WriteFile(filepath.Join(p, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func configModelOf(t *testing.T, dir string) (any, any) {
	t.Helper()
	model, provider, err := ReadConfigModel(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return model, provider
}

func TestAnEditedConfigModelIsSeenOnTheNextRead(t *testing.T) {
	p := listingProfile(t)
	if m, pr := configModelOf(t, p); m != "gpt-4o" || pr != "openai" {
		t.Fatalf("%v %v", m, pr)
	}
	if err := os.WriteFile(filepath.Join(p, "config.yaml"), []byte("model:\n  provider: anthropic\n  default: claude-opus-4.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, pr := configModelOf(t, p); m != "claude-opus-4.6" || pr != "anthropic" {
		t.Fatalf("%v %v", m, pr)
	}
}

func TestAMissingFileIsNotCachedAndIsPickedUpWhenCreated(t *testing.T) {
	ResetFileCache()
	t.Cleanup(ResetFileCache)
	bare := filepath.Join(t.TempDir(), "profiles", "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if n, v, s := ReadDistributionMeta(bare); n != nil || v != nil || s != nil {
		t.Fatalf("%v %v %v", n, v, s)
	}
	profileFileCacheMu.Lock()
	for key := range profileFileCache {
		if key.kind == "distribution" {
			profileFileCacheMu.Unlock()
			t.Fatalf("missing file cached under %v", key)
		}
	}
	profileFileCacheMu.Unlock()
	if err := os.WriteFile(filepath.Join(bare, "distribution.yaml"), []byte("name: late\nversion: 9\nsource: registry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, v, s := ReadDistributionMeta(bare); n != "late" || v != 9 || s != "registry" {
		t.Fatalf("%v %#v %v", n, v, s)
	}
}

func TestAnUnchangedFileIsParsedOnceAcrossRepeatedReads(t *testing.T) {
	p := listingProfile(t)
	var parsed []string
	old := loadYAMLDict
	t.Cleanup(func() { loadYAMLDict = old })
	loadYAMLDict = func(path string) map[string]any {
		parsed = append(parsed, filepath.Base(path))
		return old(path)
	}
	for range 3 {
		ReadProfileMeta(p)
		ReadDistributionMeta(p)
	}
	counts := map[string]int{}
	for _, name := range parsed {
		counts[name]++
	}
	if counts["profile.yaml"] != 1 || counts["distribution.yaml"] != 1 {
		t.Fatalf("%v", parsed)
	}
}

func TestTheRawConfigReaderKeepsItsUncachedContract(t *testing.T) {
	p := listingProfile(t)
	configPath := filepath.Join(p, "config.yaml")
	provider := func() any {
		raw, err := config.ReadUserConfigRaw(context.Background(), configPath)
		if err != nil {
			t.Fatal(err)
		}
		model, _ := raw["model"].(map[string]any)
		return model["provider"]
	}
	if got := provider(); got != "openai" {
		t.Fatalf("%v", got)
	}
	if err := os.WriteFile(configPath, []byte("model:\n  provider: xai\n  default: grok-4.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := provider(); got != "xai" {
		t.Fatalf("%v", got)
	}
}

func TestConfigModelCacheIsKeyedOnTheBackendVersionNotTheLocalFile(t *testing.T) {
	p := listingProfile(t)
	if m, pr := configModelOf(t, p); m != "gpt-4o" || pr != "openai" {
		t.Fatalf("%v %v", m, pr)
	}
	t.Cleanup(config.SwapConfigVersion(func(context.Context, string) ([]any, error) { return []any{"v2"}, nil }))
	t.Cleanup(config.SwapFileReadUserDoc(func(config.FileBackend, context.Context, string) (any, error) {
		return map[string]any{"model": map[string]any{"provider": "anthropic", "default": "claude-opus-4.6"}}, nil
	}))
	if m, pr := configModelOf(t, p); m != "claude-opus-4.6" || pr != "anthropic" {
		t.Fatalf("%v %v", m, pr)
	}
}
