package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const discoveryProviderSource = `from agent.memory_provider import MemoryProvider

class Provider(MemoryProvider):
    @property
    def name(self):
        return "%s"
    def is_available(self):
        return True
    def initialize(self, *a, **kw):
        pass
    def get_tool_schemas(self):
        return []

def register(ctx):
    ctx.register_memory_provider(Provider())
`

func discWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func discProvider(t *testing.T, root, name string) string {
	dir := filepath.Join(root, name)
	discWrite(t, filepath.Join(dir, "__init__.py"), strings.Replace(discoveryProviderSource, "%s", name, 1))
	return dir
}

// Installs an entry point of the memory group on the import path
func discEntryPoint(t *testing.T, name, value string) string {
	root := t.TempDir()
	discWrite(t, filepath.Join(root, name+"_dist-1.0.dist-info", "entry_points.txt"), "[icebath_agent.memory_providers]\n"+name+" = "+value+"\n")
	t.Setenv("PYTHONPATH", root)
	return root
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
