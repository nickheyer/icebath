package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Python sources the Go tree may still hold, each a slash path or a directory prefix ending in a slash
var pythonSourceAllowList = []string{
	"pkg/plugins/hostchild/",
	"pkg/plugins/testdata/",
	"pkg/pm/venv_entry.py",
	"pkg/pm/bundle_scripts/",
	"pkg/browser/browser_use_shot.py",
}

// Directory holding go.mod, found by walking upward from the package directory
func fenceModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && st.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// Reports a slash path covered by the allow list
func pythonSourceAllowed(rel string) bool {
	for _, entry := range pythonSourceAllowList {
		if rel == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(rel, entry)) {
			return true
		}
	}
	return false
}

func TestNoPythonSourceUnderPkgCmdOrThirdParty(t *testing.T) {
	root := fenceModuleRoot(t)
	for _, top := range []string{"pkg", "cmd", "third_party"} {
		start := filepath.Join(root, top)
		if _, err := os.Stat(start); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".py") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel = filepath.ToSlash(rel); !pythonSourceAllowed(rel) {
				t.Errorf("%s is Python source inside the Go tree", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPythonSourceAllowListEntriesStillExist(t *testing.T) {
	root := fenceModuleRoot(t)
	for _, entry := range pythonSourceAllowList {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(entry, "/")))); err != nil {
			t.Errorf("allow list entry %s names nothing: %v", entry, err)
		}
	}
}
