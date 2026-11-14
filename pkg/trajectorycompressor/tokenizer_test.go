package trajectorycompressor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tokenizers"
)

const layouts = "../tokenizers/testdata/layouts"

// Copies one tokenizer fixture into dst
func copyLayout(t *testing.T, name, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS(filepath.Join(layouts, name))); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTokenizerReadsALocalFolderAndTheOfflineHubCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cache := t.TempDir()
	t.Setenv("HF_HUB_CACHE", cache)
	t.Setenv("HF_HUB_OFFLINE", "1")
	repo := filepath.Join(cache, "models--moonshotai--Kimi-K2-Thinking")
	if err := os.MkdirAll(filepath.Join(repo, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "refs", "main"), []byte("abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyLayout(t, "moonshot", filepath.Join(repo, "snapshots", "abc123"))
	cached, err := LoadTokenizer(DefaultConfig().TokenizerName)
	if err != nil {
		t.Fatal(err)
	}
	local, err := LoadTokenizer(filepath.Join(layouts, "moonshot"))
	if err != nil {
		t.Fatal(err)
	}
	text := "[BOS]The quick brown fox, 中文 12345[EOS]"
	a, errA := cached.Encode(text)
	b, errB := local.Encode(text)
	if errA != nil || errB != nil || len(a) < 8 || !slices.Equal(a, b) {
		t.Fatalf("cached %v %v, local %v %v", a, errA, b, errB)
	}
	if _, err := LoadTokenizer("nobody/absent"); err == nil || !strings.Contains(err.Error(), "nobody/absent") {
		t.Fatalf("an uncached model offline gave %v", err)
	}
	c := &Compressor{tokenizer: cached}
	if n, err := c.CountTokens(text); err != nil || n != len(a) {
		t.Fatalf("CountTokens %d %v, want %d", n, err, len(a))
	}
}

func TestLoadTokenizerNamesTheFormatsForAPythonOnlyRepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tokenizer_config.json"), []byte(`{"tokenizer_class":"CustomTokenizer","auto_map":{"AutoTokenizer":["custom.CustomTokenizer",null]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTokenizer(dir)
	if err == nil || !strings.Contains(err.Error(), tokenizers.Formats) {
		t.Fatalf("error %v", err)
	}
}
