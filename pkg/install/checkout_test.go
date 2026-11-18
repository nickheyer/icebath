package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvedPathsJSONReportsHomeAndInstallDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("ICEBATH_INSTALL_DIR", "")
	out, err := ResolvedPathsJSON(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report["icebath_home"] != homeDir || report["install_dir"] != filepath.Join(homeDir, InstallDirName) {
		t.Fatalf("report %v", report)
	}
	for _, key := range []string{"long_profile_root", "normalized", "resolver", "temp"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("missing %s in %v", key, report)
		}
	}
}

func TestSetupFromCheckoutPublishesTheBuiltBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix user bin")
	}
	homeDir, userHome := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	os.WriteFile(filepath.Join(userHome, ".bashrc"), []byte("# mine\n"), 0o644)
	checkout := t.TempDir()
	os.WriteFile(InstallBinaryPath(checkout), []byte("#!/bin/sh\n"), 0o755)
	var lines []string
	if err := SetupFromCheckout(context.Background(), checkout, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(userHome, ".local", "bin", "icebath")); err != nil {
		t.Fatalf("command not published: %v (%v)", err, lines)
	}
	rc, _ := os.ReadFile(filepath.Join(userHome, ".bashrc"))
	if !strings.Contains(string(rc), `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf(".bashrc %q", rc)
	}
	if st, err := os.Stat(filepath.Join(homeDir, "skills")); err != nil || !st.IsDir() {
		t.Fatal("skills dir not seeded")
	}
	if stamp, _ := os.ReadFile(filepath.Join(checkout, InstallMethodFile)); string(stamp) != "git\n" {
		t.Fatalf("checkout stamp %q", stamp)
	}
}

func TestCopySkillsNoClobberKeepsExistingFiles(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(src, "a"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "SKILL.md"), []byte("bundled"), 0o644)
	os.WriteFile(filepath.Join(src, "b.md"), []byte("bundled"), 0o644)
	os.WriteFile(filepath.Join(dst, "b.md"), []byte("mine"), 0o644)
	if err := CopySkillsNoClobber(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "a", "SKILL.md")); string(data) != "bundled" {
		t.Fatal("bundled skill not copied")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "b.md")); string(data) != "mine" {
		t.Fatal("existing file replaced")
	}
}
