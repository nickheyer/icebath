package pm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModuleInstalledReadsPackageFoldersFilesExtensionsAndPathFiles(t *testing.T) {
	site, extra := t.TempDir(), t.TempDir()
	for _, file := range []string{"pkg/__init__.py", "pkg/sub/inner.py", "single.py", "native.cpython-314-x86_64-linux-gnu.so", "plain.pyd", "names/part/leaf.py"} {
		path := filepath.Join(site, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(extra, "linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "editable.pth"), []byte("# comment\nimport sys\n"+extra+"\nmissing-folder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := moduleRoots(site)
	if len(roots) != 2 || roots[1] != extra {
		t.Fatalf("roots %v", roots)
	}
	for module, want := range map[string]bool{
		"pkg": true, "pkg.sub": true, "pkg.sub.inner": true, "single": true, "native": true, "plain": true, "names.part.leaf": true, "linked": true,
		"absent": false, "pkg.absent": false, "single.inner": false, "pkg.sub.inner.deeper": false, "": false, "pkg.": false,
	} {
		if got := moduleInstalled(roots, module); got != want {
			t.Errorf("moduleInstalled(%q) = %v, want %v", module, got, want)
		}
	}
}
