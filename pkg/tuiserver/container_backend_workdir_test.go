package tuiserver

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
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

func TestContainerCwdIsTheSessionWorkspace(t *testing.T) {
	for _, bridged := range []bool{false, true} {
		name := "config-only"
		if bridged {
			name = "env-bridged"
		}
		t.Run(name, func(t *testing.T) {
			h := containerLaunchHome(t)
			container := containerDir(t)
			if bridged {
				t.Setenv("TERMINAL_ENV", "docker")
				t.Setenv("TERMINAL_CWD", container)
			} else {
				writeCfg(t, h, "terminal:\n  backend: docker\n  cwd: "+container+"\n")
			}
			if got := completionCwdOf(t, map[string]any{}); got != container {
				t.Fatalf("completion cwd = %q", got)
			}
			if got := terminalTaskCwd(t.Context(), nil); got != container {
				t.Fatalf("task cwd = %q", got)
			}
			session := tuigateway.NewSession(map[string]any{"cwd": completionCwdOf(t, map[string]any{}), "profile_home": nil})
			if sessionIsLocalBackend(t.Context(), session) {
				t.Fatal("container session reported local")
			}
			if got := displaySessionCwd(t.Context(), session); got != container {
				t.Fatalf("display cwd = %q", got)
			}
		})
	}
}

func TestLocalBackendMissingCwdStillFallsBack(t *testing.T) {
	h := containerLaunchHome(t)
	writeCfg(t, h, "terminal:\n  backend: local\n  cwd: "+containerDir(t)+"\n")
	wd, _ := os.Getwd()
	if got := completionCwdOf(t, map[string]any{}); got != wd {
		t.Fatalf("cwd = %q want %q", got, wd)
	}
}

func TestContainerBackendHostCwdKeepsHostPath(t *testing.T) {
	h := containerLaunchHome(t)
	project := mustMkdir(t, filepath.Join(t.TempDir(), "project"))
	writeCfg(t, h, "terminal:\n  backend: docker\n  cwd: "+project+"\n")
	if got := completionCwdOf(t, map[string]any{}); got != project {
		t.Fatalf("cwd = %q want %q", got, project)
	}
}

func TestNamedContainerProfileUsesItsContainerCwd(t *testing.T) {
	containerLaunchHome(t)
	tmp := t.TempDir()
	container := containerDir(t)
	profile := writeCfg(t, filepath.Join(tmp, "box"), "terminal:\n  backend: docker\n  cwd: "+container+"\n")
	saved := resolveProfileHome
	resolveProfileHome = func(name string) (string, error) {
		if name == "box" {
			return profile, nil
		}
		return "", nil
	}
	t.Cleanup(func() { resolveProfileHome = saved })
	inherited := mustMkdir(t, filepath.Join(tmp, "user-home"))
	if got := completionCwdOf(t, map[string]any{"profile": "box", "cwd": inherited, "cwd_explicit": false}); got != container {
		t.Fatalf("inherited cwd = %q", got)
	}
	if got := completionCwdOf(t, map[string]any{"profile": "box"}); got != container {
		t.Fatalf("profile cwd = %q", got)
	}
	if sessionIsLocalBackend(t.Context(), tuigateway.NewSession(map[string]any{"cwd": container, "profile_home": profile})) {
		t.Fatal("named container session reported local")
	}
}
