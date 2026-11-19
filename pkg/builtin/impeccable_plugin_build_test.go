package builtin

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Unpacked copy of the embedded payload archive the cases read as a tree, set by TestMain
var payloadTestDir string

func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantContains(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Errorf("missing %q in\n%s", part, text)
		}
	}
}

func wantLacks(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if strings.Contains(text, part) {
			t.Errorf("unexpected %q in\n%s", part, text)
		}
	}
}

func writeAt(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	writeTestFile(t, p, contents)
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
