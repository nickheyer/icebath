package pyimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindOriginFollowsFileFinderOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(first, "nsonly", "mod.py"), "")
	writeFile(t, filepath.Join(second, "nsonly", "__init__.py"), "")
	writeFile(t, filepath.Join(second, "pkg", "__init__.py"), "")
	writeFile(t, filepath.Join(second, "pkg", "sub", "__init__.py"), "")
	writeFile(t, filepath.Join(second, "pkg", "sub", "leaf.py"), "")
	writeFile(t, filepath.Join(second, "flat.py"), "")
	writeFile(t, filepath.Join(second, "both.py"), "")
	writeFile(t, filepath.Join(second, "both.cpython-314-x86_64-linux-gnu.so"), "")
	writeFile(t, filepath.Join(second, "bare", "x.py"), "")
	path := []string{first, second}
	cases := map[string]string{
		"nsonly":       filepath.Join(second, "nsonly", "__init__.py"),
		"pkg.sub.leaf": filepath.Join(second, "pkg", "sub", "leaf.py"),
		"pkg.sub":      filepath.Join(second, "pkg", "sub", "__init__.py"),
		"flat":         filepath.Join(second, "flat.py"),
		"flat.child":   "",
		"pkg.missing":  "",
		"bare":         "",
		"os":           "",
		"sys":          "",
		"":             "",
	}
	for module, want := range cases {
		if got := FindOrigin(path, module); got != want {
			t.Errorf("FindOrigin(%q) = %q, want %q", module, got, want)
		}
	}
	if got := FindOrigin(path, "both"); !strings.HasSuffix(got, ".so") {
		t.Errorf("extension module must win over source, got %q", got)
	}
}

func TestReadSourceStripsBOMAndCutsCodePoints(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "m.py"), "\uFEFFé\xff\xfeabc")
	if got := ReadSource(filepath.Join(dir, "m.py"), 3); got != "é\uFFFD\uFFFD" {
		t.Fatalf("got %q", got)
	}
	if ReadSource(filepath.Join(dir, "m.pyc"), 10) != "" {
		t.Fatal("bytecode origin has no source")
	}
}
