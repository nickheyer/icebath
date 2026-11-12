package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetProjectRootIsAnExistingAbsoluteDirectory(t *testing.T) {
	root := GetProjectRoot()
	if !filepath.IsAbs(root) {
		t.Fatalf("project root %q is not absolute", root)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("project root %q is not a directory: %v", root, err)
	}
}

func TestGetProjectRootIsTheSourceTreeOfThisPackage(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(wd)))
	if err != nil {
		t.Fatal(err)
	}
	if root := GetProjectRoot(); root != want {
		t.Fatalf("project root %q, the source tree is %q", root, want)
	}
	if module := goModModulePath(filepath.Join(want, "go.mod")); module != "github.com/nickheyer/icebath" {
		t.Fatalf("go.mod declares %q", module)
	}
	if _, found := trimmedSourceRoot(t.TempDir(), "github.com/nickheyer/icebath/pkg/config/facade_misc.go"); found {
		t.Fatal("a directory outside any checkout holds no source root")
	}
	if root, found := trimmedSourceRoot(wd, "github.com/nickheyer/icebath/pkg/config/facade_misc.go"); !found || root != want {
		t.Fatalf("source root from the package directory %q %v", root, found)
	}
}
