package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func discWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDefinesMemoryProviderParsesInsteadOfMatchingText(t *testing.T) {
	dir := t.TempDir()
	discWrite(t, filepath.Join(dir, "__init__.py"), "\"\"\"Mentions MemoryProvider and register_memory_provider in prose only.\"\"\"\n")
	discWrite(t, filepath.Join(dir, "broken.py"), "def (:\n")
	if definesMemoryProvider(dir) {
		t.Fatal("docstring counted as a definition")
	}
	discWrite(t, filepath.Join(dir, "impl.py"), "from agent.memory_provider import MemoryProvider as Base\n\nclass Store(Base):\n    pass\n")
	if !definesMemoryProvider(dir) {
		t.Fatal("aliased subclass not found")
	}
	other := t.TempDir()
	discWrite(t, filepath.Join(other, "__init__.py"), "\xef\xbb\xbfdef register(ctx):\n    ctx.register_memory_provider(object())\n")
	if !definesMemoryProvider(other) {
		t.Fatal("register call behind a BOM not found")
	}
}

// Exported provider probe agrees with the internal one on an empty directory
func TestDefinesMemoryProviderExported(t *testing.T) {
	dir := t.TempDir()
	if DefinesMemoryProvider(dir) != definesMemoryProvider(dir) || DefinesMemoryProvider(dir) {
		t.Fatal("empty dir defines a memory provider")
	}
}
