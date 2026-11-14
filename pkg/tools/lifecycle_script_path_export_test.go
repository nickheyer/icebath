package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// The exported resolver answers exactly as the package resolver for a present and an absent script
func TestResolveLifecycleScriptPathExportedMatchesPackageResolver(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "jobs", "nightly.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{script, filepath.Join("jobs", "nightly.sh"), filepath.Join(home, "missing.sh"), ""} {
		gotPath, gotOK := ResolveLifecycleScriptPath(home, in)
		wantPath, wantOK := resolveScriptPath(home, in)
		if gotPath != wantPath || gotOK != wantOK {
			t.Fatalf("%q: exported (%q, %v) differs from package (%q, %v)", in, gotPath, gotOK, wantPath, wantOK)
		}
	}
}
