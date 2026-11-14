//go:build !windows

package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Creates the private token directory of an ownership under home and returns the token path inside it
func sshTokenFixture(t *testing.T, tokenRoot string) string {
	t.Helper()
	tokenDir := filepath.Join(tokenRoot, strings.Repeat("a", 32))
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for dir := tokenDir; dir != filepath.Dir(tokenRoot) && len(dir) > 1; dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(tokenDir, "0123456789abcdef.token")
}

func writeSSHToken(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("b", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Asserts err is the launch exit whose message matches one of the given fragments
func wantTokenExit(t *testing.T, err error, fragments ...string) {
	t.Helper()
	var exit *DashboardExit
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v", err)
	}
	for _, fragment := range fragments {
		if strings.Contains(exit.Msg, fragment) {
			return
		}
	}
	t.Fatalf("exit message %q matches none of %v", exit.Msg, fragments)
}

func TestTokenFileIsReadAndUnlinkedThroughPrivateDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	icebathHome := filepath.Join(home, ".icebath")
	tokenPath := sshTokenFixture(t, filepath.Join(icebathHome, "desktop-ssh"))
	writeSSHToken(t, tokenPath)
	t.Setenv("ICEBATH_HOME", icebathHome)
	token, err := readSSHSessionTokenFile(tokenPath)
	if err != nil || token != strings.Repeat("b", 64) {
		t.Fatalf("token = %q err = %v", token, err)
	}
	if _, err := os.Lstat(tokenPath); err == nil {
		t.Fatal("the token file was not unlinked")
	}
}

func TestTokenAnchorIsOsHomeNotActiveProfile(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	tokenPath := sshTokenFixture(t, filepath.Join(home, ".icebath", "desktop-ssh"))
	for _, elsewhere := range []string{filepath.Join(home, ".icebath", "profiles", "coder"), filepath.Join(tmp, "opt", "data")} {
		writeSSHToken(t, tokenPath)
		t.Setenv("ICEBATH_HOME", elsewhere)
		token, err := readSSHSessionTokenFile(tokenPath)
		if err != nil || token != strings.Repeat("b", 64) {
			t.Fatalf("home %s: token = %q err = %v", elsewhere, token, err)
		}
		if _, err := os.Lstat(tokenPath); err == nil {
			t.Fatal("the token file was not unlinked")
		}
	}
}

func TestTokenUnderProfileDesktopSshIsRejected(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	profileHome := filepath.Join(home, ".icebath", "profiles", "coder")
	tokenPath := sshTokenFixture(t, filepath.Join(profileHome, "desktop-ssh"))
	writeSSHToken(t, tokenPath)
	t.Setenv("ICEBATH_HOME", profileHome)
	_, err := readSSHSessionTokenFile(tokenPath)
	wantTokenExit(t, err, "desktop-ssh directory")
}

func TestTokenFileRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	tokenPath := sshTokenFixture(t, filepath.Join(home, ".icebath", "desktop-ssh"))
	target := filepath.Join(tmp, "token")
	writeSSHToken(t, target)
	if err := os.Symlink(target, tokenPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(home, ".icebath"))
	_, err := readSSHSessionTokenFile(tokenPath)
	wantTokenExit(t, err, "symlink", "not accessible")
	if _, err := os.Lstat(tokenPath); err == nil {
		t.Fatal("the symlink was left in place")
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != strings.Repeat("b", 64) {
		t.Fatalf("target = %q err = %v", raw, err)
	}
}

func TestTokenFileRejectsParentEscape(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	tokenRoot := filepath.Join(home, ".icebath", "desktop-ssh")
	if err := os.MkdirAll(tokenRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(filepath.Dir(tokenRoot), "0123456789abcdef.token")
	writeSSHToken(t, escaped)
	t.Setenv("ICEBATH_HOME", filepath.Join(home, ".icebath"))
	_, err := readSSHSessionTokenFile(tokenRoot + "/../" + filepath.Base(escaped))
	wantTokenExit(t, err, "invalid runtime path")
	if _, err := os.Stat(escaped); err != nil {
		t.Fatal("the escaped file was removed")
	}
}

func TestWindowsRuntimeRootStaysAtMachineRootForNamedProfile(t *testing.T) {
	machineRoot := filepath.Join(t.TempDir(), "custom-icebath-root")
	t.Setenv("ICEBATH_HOME", filepath.Join(machineRoot, "profiles", "writer_2"))
	if got := sshRoot(); got != filepath.Join(machineRoot, "desktop-ssh") {
		t.Fatalf("root = %q", got)
	}
}
