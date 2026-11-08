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

func TestIsWriteDeniedAndApprovalExports(t *testing.T) {
	env := &Env{Home: t.TempDir()}
	for _, p := range []string{"/etc/shadow", filepath.Join(env.Home, ".env"), filepath.Join(t.TempDir(), "note.mp3")} {
		if got, want := IsWriteDenied(env, p), classifyWriteDenial(env, p, false) != ""; got != want {
			t.Fatalf("IsWriteDenied(%q) = %v, want %v", p, got, want)
		}
		if got, want := IsWriteApprovalRequired(env, p), isWriteApprovalRequired(env, p); got != want {
			t.Fatalf("IsWriteApprovalRequired(%q) = %v, want %v", p, got, want)
		}
	}
}
