package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/i18n"
)

// Clears the tip key and language caches around a test
func freshTipCaches(t *testing.T) {
	t.Helper()
	ResetTipsCache()
	i18n.ResetLanguageCache()
	t.Cleanup(func() {
		ResetTipsCache()
		i18n.ResetLanguageCache()
	})
}

func TestEveryTipKeyResolvesToCatalogText(t *testing.T) {
	freshTipCaches(t)
	ctx := context.Background()
	keys := tipKeys(ctx)
	if len(keys) == 0 {
		t.Fatal("the English catalog ships tips")
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if i18n.T(ctx, k, "", nil) == k {
			t.Fatalf("bare key echo for %s", k)
		}
		if seen[k] {
			t.Fatalf("duplicate key %s", k)
		}
		seen[k] = true
	}
}

func TestRandomTipAndPlaceholderComeFromTheCatalog(t *testing.T) {
	freshTipCaches(t)
	ctx := context.Background()
	tips := map[string]bool{}
	for _, k := range tipKeys(ctx) {
		tips[i18n.T(ctx, k, "", nil)] = true
	}
	if !tips[getRandomTip(ctx)] {
		t.Fatalf("tip %q not from the catalog", getRandomTip(ctx))
	}
	prompts := map[string]bool{}
	for _, k := range composerHintKeys(ctx) {
		prompts[i18n.T(ctx, k, "", nil)] = true
	}
	if !prompts[getRandomComposerHint(ctx)] {
		t.Fatalf("hint %q not from the catalog", getRandomComposerHint(ctx))
	}
}

func TestPickersFollowActiveLanguageWithEnglishFallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "en.yaml"), []byte("tips:\n  t001: 'first'\n  t002: 'second'\n  placeholder:\n    p01: 'ask'\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "de.yaml"), []byte("tips:\n  t001: 'erstens'\n"), 0o600)
	t.Setenv("ICEBATH_BUNDLED_LOCALES", dir)
	t.Setenv("ICEBATH_LANGUAGE", "de")
	freshTipCaches(t)
	ctx := context.Background()
	if keys := tipKeys(ctx); len(keys) != 2 || keys[0] != "tips.t001" || keys[1] != "tips.t002" {
		t.Fatalf("tip keys %v", keys)
	}
	if keys := composerHintKeys(ctx); len(keys) != 1 || keys[0] != "tips.placeholder.p01" {
		t.Fatalf("hint keys %v", keys)
	}
	seen := map[string]bool{}
	for range 50 {
		seen[getRandomTip(ctx)] = true
	}
	if len(seen) != 2 || !seen["erstens"] || !seen["second"] {
		t.Fatalf("seen %v", seen)
	}
	if got := getRandomComposerHint(ctx); got != "ask" {
		t.Fatalf("prompt %q", got)
	}
}
