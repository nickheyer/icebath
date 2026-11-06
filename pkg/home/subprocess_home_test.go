package home

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Host or container mode with TERMINAL_HOME_MODE and ICEBATH_REAL_HOME unset
func homeMode(t *testing.T, container bool) {
	t.Helper()
	swap(t, &detectContainer, func() bool { return container })
	for _, key := range []string{"TERMINAL_HOME_MODE", "ICEBATH_REAL_HOME"} {
		dropEnv(t, key)
	}
}

func dropEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

func mkdirAll(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGetSubprocessHome(t *testing.T) {
	ctx := context.Background()
	t.Run("host_auto_keeps_real_home_when_profile_home_exists", func(t *testing.T) {
		homeMode(t, false)
		real := filepath.Join(t.TempDir(), "real-home")
		h := filepath.Join(real, ".icebath", "profiles", "coder")
		mkdirAll(t, filepath.Join(h, "home"))
		t.Setenv("HOME", real)
		t.Setenv(EnvHome, h)
		if got := SubprocessHome(ctx, nil); got != "" {
			t.Fatal(got)
		}
	})
	t.Run("host_auto_repairs_missing_home", func(t *testing.T) {
		homeMode(t, false)
		real := filepath.Join(t.TempDir(), "real-home")
		h := filepath.Join(real, ".icebath", "profiles", "coder")
		mkdirAll(t, filepath.Join(h, "home"))
		dropEnv(t, "HOME")
		t.Setenv("ICEBATH_REAL_HOME", real)
		t.Setenv(EnvHome, h)
		if got := SubprocessHome(ctx, nil); got != real {
			t.Fatal(got)
		}
	})
	t.Run("container_auto_uses_profile_home_when_home_dir_exists", func(t *testing.T) {
		homeMode(t, true)
		h := filepath.Join(t.TempDir(), ".icebath")
		profile := mkdirAll(t, filepath.Join(h, "home"))
		t.Setenv(EnvHome, h)
		if got := SubprocessHome(ctx, nil); got != profile {
			t.Fatal(got)
		}
	})
	t.Run("returns_profile_specific_path", func(t *testing.T) {
		homeMode(t, false)
		dir := filepath.Join(t.TempDir(), ".icebath", "profiles", "coder")
		profile := mkdirAll(t, filepath.Join(dir, "home"))
		t.Setenv("TERMINAL_HOME_MODE", "profile")
		t.Setenv(EnvHome, dir)
		if got := SubprocessHome(ctx, nil); got != profile {
			t.Fatal(got)
		}
	})
	t.Run("real_mode_repairs_parent_home_already_pointing_at_profile", func(t *testing.T) {
		homeMode(t, false)
		tmp := t.TempDir()
		dir := filepath.Join(tmp, ".icebath", "profiles", "coder")
		profile := mkdirAll(t, filepath.Join(dir, "home"))
		real := mkdirAll(t, filepath.Join(tmp, "real-home"))
		t.Setenv("TERMINAL_HOME_MODE", "real")
		t.Setenv(EnvHome, dir)
		t.Setenv("HOME", profile)
		t.Setenv("ICEBATH_REAL_HOME", real)
		if got := RealHome(ctx, nil); got != real {
			t.Fatal(got)
		}
		if got := SubprocessHome(ctx, nil); got != real {
			t.Fatal(got)
		}
	})
	t.Run("two_profiles_get_different_homes", func(t *testing.T) {
		homeMode(t, true)
		base := filepath.Join(t.TempDir(), ".icebath", "profiles")
		for _, name := range []string{"alpha", "beta"} {
			mkdirAll(t, filepath.Join(base, name, "home"))
		}
		t.Setenv(EnvHome, filepath.Join(base, "alpha"))
		a := SubprocessHome(ctx, nil)
		t.Setenv(EnvHome, filepath.Join(base, "beta"))
		b := SubprocessHome(ctx, nil)
		if a == "" || b == "" || a == b || filepath.Base(filepath.Dir(a)) != "alpha" || filepath.Base(a) != "home" ||
			filepath.Base(filepath.Dir(b)) != "beta" || filepath.Base(b) != "home" {
			t.Fatalf("%q %q", a, b)
		}
	})
	t.Run("context_override_wins_over_the_child_env", func(t *testing.T) {
		homeMode(t, true)
		tmp := t.TempDir()
		over := mkdirAll(t, filepath.Join(tmp, "over", "home"))
		mkdirAll(t, filepath.Join(tmp, "child", "home"))
		got := SubprocessHome(WithOverride(ctx, filepath.Dir(over)), map[string]string{EnvHome: filepath.Join(tmp, "child")})
		if got != over {
			t.Fatal(got)
		}
	})
}

func TestApplySubprocessHomeEnvSetsHomesAndScratch(t *testing.T) {
	homeMode(t, false)
	tmp := t.TempDir()
	real := mkdirAll(t, filepath.Join(tmp, "real-home"))
	h := mkdirAll(t, filepath.Join(tmp, ".icebath"))
	dropEnv(t, "HOME")
	t.Setenv("ICEBATH_REAL_HOME", real)
	env := map[string]string{EnvHome: h}
	ApplySubprocessHomeEnv(context.Background(), env)
	if env["HOME"] != real || env["ICEBATH_REAL_HOME"] != real || env["TMPDIR"] != filepath.Join(h, "cache", "scratch") {
		t.Fatalf("%v", env)
	}
}
