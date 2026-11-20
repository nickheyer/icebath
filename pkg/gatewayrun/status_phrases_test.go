package gatewayrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// test_status_phrases.py test_status_phrase_does_not_leak_raw_preview_or_args
func TestStatusPhraseDoesNotLeakRawPreviewOrArgs(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	msg := chooseStatusPhrase("status", nil, func(n int) int { return 4 % n }, nil)
	if strings.Contains(msg, "actual private scratch") || strings.Contains(msg, "SECRET-123") || msg == "" {
		t.Fatalf("msg = %q", msg)
	}
}

// test_status_phrases.py test_status_phrase_path_can_load_relative_directory
func TestStatusPhrasePathCanLoadRelativeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	phraseDir := filepath.Join(home, "phrase-catalog")
	if err := os.MkdirAll(phraseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(phraseDir, "01-status.yaml"), []byte("status:\n  - relative dir status text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := resolveStatusPhraseCatalog(home, map[string]any{"display": map[string]any{"status_phrases": map[string]any{"path": "phrase-catalog"}}}, "whatsapp")
	if !slices.Contains(catalog["status"], "relative dir status text") {
		t.Fatalf("status phrases = %#v", catalog["status"])
	}
}

// test_status_phrases.py test_choose_status_phrase_uses_custom_catalog_without_leaking_args
func TestChooseStatusPhraseUsesCustomCatalogWithoutLeakingArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	catalog := resolveStatusPhraseCatalog(home, map[string]any{"display": map[string]any{"status_phrases": map[string]any{
		"mode": "replace", "status": []any{"custom safe status text"}}}}, "whatsapp")
	msg := chooseStatusPhrase("status", nil, nil, catalog)
	if msg != "custom safe status text" || strings.Contains(msg, "SECRET") {
		t.Fatalf("msg = %q", msg)
	}
}
