package home

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileHasServableIdentity(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  bool
	}{
		{"no markers", func(t *testing.T, dir string) {
			writeMarker(t, filepath.Join(dir, "logs.txt"), "x")
		}, false},
		{"empty env only", func(t *testing.T, dir string) {
			writeMarker(t, filepath.Join(dir, ".env"), "")
		}, false},
		{"non empty env", func(t *testing.T, dir string) {
			writeMarker(t, filepath.Join(dir, ".env"), "BETA=1\n")
		}, true},
		{"empty config counts", func(t *testing.T, dir string) {
			writeMarker(t, filepath.Join(dir, "config.yaml"), "")
		}, true},
		{"empty env plus soul", func(t *testing.T, dir string) {
			writeMarker(t, filepath.Join(dir, ".env"), "")
			writeMarker(t, filepath.Join(dir, "SOUL.md"), "")
		}, true},
		{"symlinked empty env", func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "empty.env")
			writeMarker(t, target, "")
			linkMarker(t, target, filepath.Join(dir, ".env"))
		}, true},
		{"dangling symlink marker", func(t *testing.T, dir string) {
			linkMarker(t, filepath.Join(dir, "gone.db"), filepath.Join(dir, "state.db"))
		}, true},
		{"marker is a directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "auth.json"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"missing home", func(t *testing.T, dir string) {
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "worker")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, dir)
			if got := ProfileHasServableIdentity(dir); got != tc.want {
				t.Fatalf("ProfileHasServableIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestServableIdentityStricterThanIdentity(t *testing.T) {
	dir := t.TempDir()
	writeMarker(t, filepath.Join(dir, ".env"), "")
	if !ProfileHasIdentity(dir) {
		t.Fatal("an empty .env is still an identity claim")
	}
	if ProfileHasServableIdentity(dir) {
		t.Fatal("an empty .env alone is not a servable identity")
	}
}

func writeMarker(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func linkMarker(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s: %v", link, err)
	}
}
