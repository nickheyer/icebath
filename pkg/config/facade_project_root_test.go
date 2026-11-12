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
