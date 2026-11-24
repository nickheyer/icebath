package tuiserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Writes a profile home whose config.yaml sets terminal cwd, or an empty config for none
func writeProfileCfg(t *testing.T, h string, cwd *string) string {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "{}\n"
	if cwd != nil {
		body = "terminal:\n  cwd: '" + *cwd + "'\n"
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// Points the launch home at a directory for the test under the lock the lease watcher reads it with
func setLaunchHome(t *testing.T, h string) {
	t.Helper()
	servedProfileMu.Lock()
	saved := icebathHome
	icebathHome = h
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		servedProfileMu.Lock()
		icebathHome = saved
		servedProfileMu.Unlock()
	})
}

func strp(s string) *string { return &s }

func mustMkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Completion cwd of a request served in the profile at h
func completionCwdOf(t *testing.T, h string) string {
	t.Helper()
	cwd, err := completionCwd(home.WithOverride(t.Context(), h))
	if err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestNamedProfilePlaceholderCwdFallsBackToOwnHome(t *testing.T) {
	tmp := t.TempDir()
	h := writeProfileCfg(t, filepath.Join(tmp, "home-beta"), strp("."))
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	launchHomeDir := writeProfileCfg(t, filepath.Join(tmp, "launch-home"), strp(launchWS))
	staleEnv := mustMkdir(t, filepath.Join(tmp, "stale-env"))
	t.Setenv("TERMINAL_CWD", staleEnv)
	setLaunchHome(t, launchHomeDir)
	if got := completionCwdOf(t, h); got != h {
		t.Fatalf("named profile cwd = %q want %q", got, h)
	}
	if got := completionCwdOf(t, launchHomeDir); got != launchWS {
		t.Fatalf("launch cwd = %q want %q", got, launchWS)
	}
}

func TestNamedProfileUnsetCwdFallsBackToOwnHome(t *testing.T) {
	tmp := t.TempDir()
	h := writeProfileCfg(t, filepath.Join(tmp, "home-plain"), nil)
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	t.Setenv("TERMINAL_CWD", launchWS)
	if got := completionCwdOf(t, h); got != h {
		t.Fatalf("cwd = %q want %q", got, h)
	}
}

func TestNamedProfileConfiguredCwdStillWins(t *testing.T) {
	tmp := t.TempDir()
	profileWS := mustMkdir(t, filepath.Join(tmp, "products"))
	h := writeProfileCfg(t, filepath.Join(tmp, "home-dev"), strp(profileWS))
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	t.Setenv("TERMINAL_CWD", launchWS)
	if got := completionCwdOf(t, h); got != profileWS {
		t.Fatalf("cwd = %q want %q", got, profileWS)
	}
}
