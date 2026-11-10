//go:build !windows

package home

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Permission bits including setuid, setgid and sticky of path
func sMode(t *testing.T, path string) uint32 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint32(st.Mode) & 0o7777
}

// Mode the host keeps after a 2770 chmod, setgid possibly cleared by the filesystem
func nativeModeAfterChmod(t *testing.T, path string) uint32 {
	t.Helper()
	if err := os.Chmod(path, 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	return sMode(t, path)
}

// Home pointed under tmp with every mode, managed and container override cleared
func isolatePolicyEnv(t *testing.T, tmp string) {
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	for _, v := range []string{"ICEBATH_HOME_MODE", "ICEBATH_MANAGED", "ICEBATH_CONTAINER", "ICEBATH_SKIP_CHMOD", "ICEBATH_UID", "ICEBATH_GID"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	swap(t, &detectContainer, func() bool { return false })
}

func mkdirs(t *testing.T, p string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if mode != 0 {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestScratchDirPermissionPolicy(t *testing.T) {
	t.Run("default is owner only", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		if got := sMode(t, testScratch(t, tmp)); got != 0o700 {
			t.Fatalf("mode %o", got)
		}
	})
	t.Run("explicit home mode uses native chmod semantics", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		expected := nativeModeAfterChmod(t, mkdirs(t, filepath.Join(tmp, "mode-probe"), 0))
		t.Setenv("ICEBATH_HOME_MODE", "2770")
		if got := sMode(t, testScratch(t, tmp)); got != expected {
			t.Fatalf("mode %o, want %o", got, expected)
		}
	})
	t.Run("managed env leaves preexisting mode untouched", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		t.Setenv("ICEBATH_MANAGED", "nixos")
		expected := nativeModeAfterChmod(t, mkdirs(t, filepath.Join(tmp, "cache", "scratch"), 0))
		if got := sMode(t, testScratch(t, tmp)); got != expected {
			t.Fatalf("mode %o, want %o", got, expected)
		}
	})
	// Home with a planted marker whose preexisting scratch mode must survive get_scratch_dir
	markerKeepsMode := func(t *testing.T, marker func(string)) {
		tmp := t.TempDir()
		h := mkdirs(t, filepath.Join(tmp, "home"), 0)
		isolatePolicyEnv(t, tmp)
		marker(filepath.Join(h, ".managed"))
		expected := nativeModeAfterChmod(t, mkdirs(t, filepath.Join(h, "cache", "scratch"), 0))
		if got := sMode(t, testScratch(t, h)); got != expected {
			t.Fatalf("mode %o, want %o", got, expected)
		}
	}
	t.Run("managed marker file leaves preexisting mode untouched", func(t *testing.T) {
		markerKeepsMode(t, func(p string) { _ = os.WriteFile(p, []byte("nixos"), 0o644) })
	})
	t.Run("empty managed marker counts as managed", func(t *testing.T) {
		markerKeepsMode(t, func(p string) { _ = os.WriteFile(p, nil, 0o644) })
	})
	t.Run("unreadable managed marker counts as managed", func(t *testing.T) {
		markerKeepsMode(t, func(p string) { _ = os.Mkdir(p, 0o755) })
	})
	t.Run("effective home marker does not govern another homes scratch", func(t *testing.T) {
		tmp := t.TempDir()
		selected := mkdirs(t, filepath.Join(tmp, "home"), 0)
		isolatePolicyEnv(t, tmp)
		_ = os.WriteFile(filepath.Join(selected, ".managed"), []byte("nixos"), 0o644)
		other := filepath.Join(tmp, "other")
		mkdirs(t, filepath.Join(other, "cache", "scratch"), 0o750)
		if got := sMode(t, testScratch(t, other)); got != 0o700 {
			t.Fatalf("mode %o", got)
		}
	})
	t.Run("canonical container signal without env override keeps operator mode", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		swap(t, &detectContainer, func() bool { return true })
		mkdirs(t, filepath.Join(tmp, "cache", "scratch"), 0o750)
		if got := sMode(t, testScratch(t, tmp)); got != 0o750 {
			t.Fatalf("mode %o", got)
		}
	})
	t.Run("repeated calls do not strip filesystem supported mode bits", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		expected := nativeModeAfterChmod(t, mkdirs(t, filepath.Join(tmp, "mode-probe"), 0))
		t.Setenv("ICEBATH_HOME_MODE", "2770")
		first, second := testScratch(t, tmp), testScratch(t, tmp)
		if first != second || sMode(t, second) != expected {
			t.Fatalf("mode %o, want %o", sMode(t, second), expected)
		}
	})
	t.Run("container keeps operator mode but honors explicit", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		t.Setenv("ICEBATH_CONTAINER", "1")
		mkdirs(t, filepath.Join(tmp, "cache", "scratch"), 0o750)
		if got := sMode(t, testScratch(t, tmp)); got != 0o750 {
			t.Fatalf("mode %o", got)
		}
		expected := nativeModeAfterChmod(t, mkdirs(t, filepath.Join(tmp, "mode-probe"), 0))
		t.Setenv("ICEBATH_HOME_MODE", "2770")
		if got := sMode(t, testScratch(t, tmp)); got != expected {
			t.Fatalf("mode %o, want %o", got, expected)
		}
	})
	t.Run("hermes uid gid applied to scratch", func(t *testing.T) {
		tmp := t.TempDir()
		isolatePolicyEnv(t, tmp)
		t.Setenv("ICEBATH_UID", "1000")
		t.Setenv("ICEBATH_GID", "911")
		type call struct {
			path     string
			uid, gid int
		}
		var calls []call
		swap(t, &chownPath, func(p string, uid, gid int) error { calls = append(calls, call{p, uid, gid}); return nil })
		testScratch(t, tmp)
		if len(calls) != 1 || calls[0] != (call{filepath.Join(tmp, "cache", "scratch"), 1000, 911}) {
			t.Fatalf("chown calls %v", calls)
		}
	})
}

func TestManagedSystemMarkers(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_MANAGED", "")
	if got := ManagedSystem(h); got != "" {
		t.Fatalf("unmanaged home reported %q", got)
	}
	for env, want := range map[string]string{"true": "nixos", "YES": "nixos", "false": "", "off": "", "brew": "", "Homebrew": "", " nix ": "nix"} {
		t.Setenv("ICEBATH_MANAGED", env)
		if got := ManagedSystem(h); got != want {
			t.Fatalf("ICEBATH_MANAGED=%q gave %q, want %q", env, got, want)
		}
	}
}
