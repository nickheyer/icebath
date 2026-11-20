package trajectorycompressor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tokenizers"
)

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
