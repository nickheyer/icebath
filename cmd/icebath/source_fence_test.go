package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Extensions of interpreted source the product never carries, test data included
var fencedSourceExtensions = map[string]string{
	".py": "Python", ".js": "JavaScript", ".mjs": "JavaScript", ".cjs": "JavaScript", ".ts": "TypeScript", ".tsx": "TypeScript",
}

// Paths excused from the fence, empty because the Go tree holds no interpreted source
var fencedSourceAllowList = []string{}

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

func fencedSourceAllowed(rel string) bool {
	for _, entry := range fencedSourceAllowList {
		if rel == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(rel, entry)) {
			return true
		}
	}
	return false
}

func TestNoInterpretedSourceUnderPkgCmdOrThirdParty(t *testing.T) {
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
			language, fenced := fencedSourceExtensions[strings.ToLower(filepath.Ext(d.Name()))]
			if d.IsDir() || !fenced {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			if rel = filepath.ToSlash(rel); !fencedSourceAllowed(rel) {
				t.Errorf("%s is %s source inside the Go tree", rel, language)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestFenceAllowListIsEmpty(t *testing.T) {
	if len(fencedSourceAllowList) != 0 {
		t.Fatalf("the fence excuses %v, the Go tree holds no interpreted source", fencedSourceAllowList)
	}
}
