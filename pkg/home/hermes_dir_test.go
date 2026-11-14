package home

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHermesDirPrefersPopulatedLegacyDir(t *testing.T) {
	h := t.TempDir()
	newPath := filepath.Join(h, "platforms", "whatsapp", "session")
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != newPath {
		t.Fatalf("missing legacy %s", got)
	}
	legacy := filepath.Join(h, "whatsapp", "session")
	os.MkdirAll(legacy, 0o755)
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != newPath {
		t.Fatalf("empty legacy must not shadow %s", got)
	}
	os.WriteFile(filepath.Join(legacy, "creds.json"), []byte("{}"), 0o644)
	if got := HermesDir(h, "platforms/whatsapp/session", "whatsapp/session"); got != legacy {
		t.Fatalf("populated legacy %s", got)
	}
	os.Symlink(filepath.Join(h, "nowhere"), filepath.Join(h, "dangling"))
	if got := HermesDir(h, "fresh", "dangling"); got != filepath.Join(h, "fresh") {
		t.Fatalf("dangling symlink %s", got)
	}
	os.WriteFile(filepath.Join(h, "plainfile"), nil, 0o644)
	if got := HermesDir(h, "fresh", "plainfile"); got != filepath.Join(h, "plainfile") {
		t.Fatalf("legacy file %s", got)
	}
	notADir := filepath.Join(h, "plainfile", "child")
	if runtime.GOOS == "windows" {
		notADir = filepath.Join(h, "fresh")
	}
	if got := HermesDir(h, "fresh", "plainfile/child"); got != notADir {
		t.Fatalf("a file parent is NotADirectoryError on POSIX and FileNotFoundError on Windows %s", got)
	}
}

func TestGetHermesDir(t *testing.T) {
	setHome := func(t *testing.T) string {
		h := t.TempDir()
		t.Setenv(EnvHome, h)
		return Dir(context.Background())
	}
	t.Run("neither_exists_returns_new", func(t *testing.T) {
		h := setHome(t)
		if got := HermesDir(h, "platforms/pairing", "pairing"); got != filepath.Join(h, "platforms", "pairing") {
			t.Fatal(got)
		}
	})
	t.Run("legacy_is_file_treated_as_content", func(t *testing.T) {
		h := setHome(t)
		legacy := filepath.Join(h, "image_cache")
		if err := os.WriteFile(legacy, []byte("sentinel"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := HermesDir(h, "cache/images", "image_cache"); got != legacy {
			t.Fatal(got)
		}
	})
	t.Run("dangling_legacy_symlink_returns_new", func(t *testing.T) {
		h := setHome(t)
		if err := os.Symlink(filepath.Join(h, "does-not-exist"), filepath.Join(h, "pairing")); err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(h, "platforms", "pairing")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fresh, "discord-approved.json"), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := HermesDir(h, "platforms/pairing", "pairing"); got != fresh {
			t.Fatal(got)
		}
	})
	t.Run("symlink_to_populated_dir_returns_legacy", func(t *testing.T) {
		h := setHome(t)
		real := filepath.Join(h, "real_store")
		if err := os.Mkdir(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, "cached.png"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		legacy := filepath.Join(h, "image_cache")
		if err := os.Symlink(real, legacy); err != nil {
			t.Fatal(err)
		}
		if got := HermesDir(h, "cache/images", "image_cache"); got != legacy {
			t.Fatal(got)
		}
	})
}
