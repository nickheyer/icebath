package photon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"context"
)

func seedSource(t *testing.T, source string, withNodeModules bool) {
	t.Helper()
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range mirrorFiles {
		writeText(t, filepath.Join(source, name), "// "+name+"\n")
	}
	if withNodeModules {
		if err := os.Mkdir(filepath.Join(source, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeText(t, filepath.Join(source, "node_modules", ".package-lock.json"), "{}")
	}
}

func writeText(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func freezeWritability(t *testing.T, writable bool) {
	orig := dirWritable
	dirWritable = func(string) bool { return writable }
	t.Cleanup(func() { dirWritable = orig })
}

func TestEnvOverrideWins(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom")
	t.Setenv("PHOTON_SIDECAR_DIR", override)
	if got := ResolveSidecarDir(context.Background(), filepath.Join(t.TempDir(), "src")); got != override {
		t.Fatalf("got %s", got)
	}
}

func TestWritableSourceRunsInPlace(t *testing.T) {
	t.Setenv("PHOTON_SIDECAR_DIR", "")
	source := filepath.Join(t.TempDir(), "src")
	seedSource(t, source, false)
	freezeWritability(t, true)
	if got := ResolveSidecarDir(context.Background(), source); got != source {
		t.Fatalf("got %s", got)
	}
}

func TestReadonlySourceWithBakedFreshDepsRunsInPlace(t *testing.T) {
	t.Setenv("PHOTON_SIDECAR_DIR", "")
	source := filepath.Join(t.TempDir(), "src")
	seedSource(t, source, true)
	if err := os.Chtimes(filepath.Join(source, "package-lock.json"), time.Unix(1000, 0), time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "node_modules", ".package-lock.json"), time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	freezeWritability(t, false)
	if got := ResolveSidecarDir(context.Background(), source); got != source {
		t.Fatalf("got %s", got)
	}
}

func TestMirrorRefreshUpdatesChangedFilesAndKeepsNodeModules(t *testing.T) {
	t.Setenv("PHOTON_SIDECAR_DIR", "")
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	source := filepath.Join(tmp, "src")
	seedSource(t, source, false)
	freezeWritability(t, false)
	ctx := context.Background()
	mirror := ResolveSidecarDir(ctx, source)
	if mirror != filepath.Join(tmp, "home", "photon", "sidecar") {
		t.Fatalf("mirror = %s", mirror)
	}
	if err := os.Mkdir(filepath.Join(mirror, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(mirror, "node_modules", "installed.txt"), "x")
	writeText(t, filepath.Join(source, "index.mjs"), "// index.mjs v2\n")
	if got := ResolveSidecarDir(ctx, source); got != mirror {
		t.Fatalf("resolved = %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(mirror, "index.mjs")); string(b) != "// index.mjs v2\n" {
		t.Fatalf("index.mjs = %q", b)
	}
	if _, err := os.Stat(filepath.Join(mirror, "node_modules", "installed.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestDirWritableProbe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	tmp := t.TempDir()
	if !DirWritable(tmp) {
		t.Fatal("temp dir not writable")
	}
	ro := filepath.Join(tmp, "ro")
	if err := os.Mkdir(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o755)
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	if DirWritable(ro) {
		t.Fatal("read only dir reported writable")
	}
}
