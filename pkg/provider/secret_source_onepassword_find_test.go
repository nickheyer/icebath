//go:build !windows

package provider

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindOpPinnedPathNotOnPath(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pinned := filepath.Join(t.TempDir(), "op")
	if err := os.WriteFile(pinned, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	onPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(onPath, "op"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)
	if got := FindOp(pinned); got != pinned {
		t.Fatalf("FindOp = %q want %q", got, pinned)
	}
}

func TestFindOpAcceptsWhatOsAccessAccepts(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "op.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindOp(dir); got != dir {
		t.Fatalf("an executable directory pinned = %q, os.access accepts it", got)
	}
	plain := filepath.Join(t.TempDir(), "op")
	if err := os.WriteFile(plain, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindOp(plain); got != "" {
		t.Fatalf("a pinned file without the execute bit = %q", got)
	}
	if got := FindOp(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("a missing pinned path = %q", got)
	}
}
