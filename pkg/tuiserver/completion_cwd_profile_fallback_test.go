package tuiserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
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

// Swaps the profile home resolver for one answering the given home for any named profile
func fakeProfileHome(t *testing.T, h string) {
	t.Helper()
	saved := resolveProfileHome
	resolveProfileHome = func(name string) (string, error) {
		if name != "" {
			return h, nil
		}
		return "", nil
	}
	t.Cleanup(func() { resolveProfileHome = saved })
}

// Points the launch home at a directory for the test under the lock the lease watcher reads it with
func setLaunchHome(t *testing.T, h string) {
	t.Helper()
	servedProfileMu.Lock()
	saved := hermesHome
	hermesHome = h
	servedProfileMu.Unlock()
	t.Cleanup(func() {
		servedProfileMu.Lock()
		hermesHome = saved
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

func completionCwdOf(t *testing.T, params map[string]any) string {
	t.Helper()
	cwd, err := completionCwd(t.Context(), params)
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
	fakeProfileHome(t, h)
	if got := completionCwdOf(t, map[string]any{"profile": "beta"}); got != h {
		t.Fatalf("named profile cwd = %q want %q", got, h)
	}
	if got := completionCwdOf(t, map[string]any{}); got != launchWS {
		t.Fatalf("launch cwd = %q want %q", got, launchWS)
	}
}

func TestNamedProfileUnsetCwdFallsBackToOwnHome(t *testing.T) {
	tmp := t.TempDir()
	h := writeProfileCfg(t, filepath.Join(tmp, "home-plain"), nil)
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	t.Setenv("TERMINAL_CWD", launchWS)
	fakeProfileHome(t, h)
	if got := completionCwdOf(t, map[string]any{"profile": "plain"}); got != h {
		t.Fatalf("cwd = %q want %q", got, h)
	}
}

func TestNamedProfileExplicitPickStillWins(t *testing.T) {
	tmp := t.TempDir()
	h := writeProfileCfg(t, filepath.Join(tmp, "home-beta"), strp("."))
	explicit := mustMkdir(t, filepath.Join(tmp, "explicit"))
	fakeProfileHome(t, h)
	if got := completionCwdOf(t, map[string]any{"profile": "beta", "cwd": explicit, "cwd_explicit": true}); got != explicit {
		t.Fatalf("cwd = %q want %q", got, explicit)
	}
}

func TestNamedProfileConfiguredCwdStillWins(t *testing.T) {
	tmp := t.TempDir()
	profileWS := mustMkdir(t, filepath.Join(tmp, "products"))
	h := writeProfileCfg(t, filepath.Join(tmp, "home-dev"), strp(profileWS))
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	t.Setenv("TERMINAL_CWD", launchWS)
	fakeProfileHome(t, h)
	if got := completionCwdOf(t, map[string]any{"profile": "dev"}); got != profileWS {
		t.Fatalf("cwd = %q want %q", got, profileWS)
	}
}

func TestTerminalTaskCwdNamedProfilePlaceholderUsesOwnHome(t *testing.T) {
	tmp := t.TempDir()
	h := writeProfileCfg(t, filepath.Join(tmp, "home-beta"), strp("."))
	launchWS := mustMkdir(t, filepath.Join(tmp, "launch-repo"))
	t.Setenv("TERMINAL_CWD", launchWS)
	fakeProfileHome(t, h)
	cwd := completionCwdOf(t, map[string]any{"profile": "beta"})
	if cwd != h {
		t.Fatalf("cwd = %q want %q", cwd, h)
	}
	session := tuigateway.NewSession(map[string]any{"cwd": cwd, "profile_home": h, "explicit_cwd": false})
	if got := terminalTaskCwd(t.Context(), session); got != h {
		t.Fatalf("task cwd = %q want %q", got, h)
	}
}
