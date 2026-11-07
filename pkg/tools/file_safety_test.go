package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveActiveProfileName(t *testing.T) {
	ctx := context.Background()
	t.Run("default when home is root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), ".icebath")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", root)
		if got := ResolveActiveProfileName(ctx); got != "default" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("named profile under the root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), ".icebath")
		profile := filepath.Join(root, "profiles", "coder")
		if err := os.MkdirAll(profile, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", profile)
		if got := ResolveActiveProfileName(ctx); got != "coder" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("falls back to default on resolution failure", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "bad\xed\xa0\x80"))
		if got := ResolveActiveProfileName(ctx); got != "default" {
			t.Fatalf("got %q", got)
		}
	})
}
