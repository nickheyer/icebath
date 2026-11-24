package tuiserver

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// Absolute path that exists only inside a sandbox, never on this host
func containerDir(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	path := "/icebath-test-workspace-" + hex.EncodeToString(buf)
	if isDir(path) {
		t.Fatalf("%s exists on the host", path)
	}
	return path
}

// Writes a config.yaml body into a home
func writeCfg(t *testing.T, h, body string) string {
	t.Helper()
	mustMkdir(t, h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// Temp launch home the gateway reads, terminal env cleared and the process cwd moved to a user home
func containerLaunchHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := mustMkdir(t, filepath.Join(tmp, "icebath-home"))
	t.Setenv("ICEBATH_HOME", h)
	setLaunchHome(t, h)
	t.Setenv("TERMINAL_ENV", "")
	os.Unsetenv("TERMINAL_ENV")
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	t.Chdir(mustMkdir(t, filepath.Join(tmp, "user-home")))
	return h
}

func TestLocalBackendMissingCwdStillFallsBack(t *testing.T) {
	h := containerLaunchHome(t)
	writeCfg(t, h, "terminal:\n  backend: local\n  cwd: "+containerDir(t)+"\n")
	wd, _ := os.Getwd()
	if got := completionCwdOf(t, h); got != wd {
		t.Fatalf("cwd = %q want %q", got, wd)
	}
}

func TestContainerBackendHostCwdKeepsHostPath(t *testing.T) {
	h := containerLaunchHome(t)
	project := mustMkdir(t, filepath.Join(t.TempDir(), "project"))
	writeCfg(t, h, "terminal:\n  backend: docker\n  cwd: "+project+"\n")
	if got := completionCwdOf(t, h); got != project {
		t.Fatalf("cwd = %q want %q", got, project)
	}
}
