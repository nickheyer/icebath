package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// test_web_host_terminal_locale.py test_lc_ctype_rule
func TestLcCtypeRule(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		env      map[string]string
		platform string
		want     string
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, "linux", ""},
		{map[string]string{}, "linux", "C.UTF-8"},
		{map[string]string{"LANG": ""}, "linux", "C.UTF-8"},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_CTYPE": "de_DE.UTF-8"}, "linux", ""},
		{map[string]string{"LC_ALL": "de_DE.UTF-8"}, "linux", ""},
		{map[string]string{"LC_CTYPE": "de_DE.UTF-8"}, "darwin", ""},
		{map[string]string{}, "darwin", "UTF-8"},
		{map[string]string{"LANG": "en_US.UTF-8"}, "darwin", "UTF-8"},
		{map[string]string{}, "win32", ""},
	}
	for _, c := range cases {
		got, ok := terminalLCCtype(c.env, c.platform)
		if ok != (c.want != "") || got != c.want {
			t.Fatalf("%v %s: got %q %v want %q", c.env, c.platform, got, ok, c.want)
		}
	}
}

// test_web_host_terminal_locale.py test_linux_host_shell_env_selects_a_utf8_locale
func TestLinuxHostShellEnvSelectsAUtf8Locale(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	for _, lang := range []string{"C.UTF-8", ""} {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		for _, kv := range os.Environ() {
			key, _, _ := strings.Cut(kv, "=")
			if strings.HasPrefix(key, "LC_") || key == "LANG" || key == "LANGUAGE" {
				t.Setenv(key, "")
				os.Unsetenv(key)
			}
		}
		if lang != "" {
			t.Setenv("LANG", lang)
		}
		_, _, env, _, err := resolveHostTerminalArgv(t.Context(), home.Dir(t.Context()), "")
		if err != nil {
			t.Fatal(err)
		}
		probe := exec.Command("locale", "charmap")
		for key, value := range env {
			probe.Env = append(probe.Env, key+"="+value)
		}
		var stderr strings.Builder
		probe.Stderr = &stderr
		out, _ := probe.Output()
		if strings.Contains(stderr.String(), "Cannot set") {
			t.Fatal(stderr.String())
		}
		if strings.TrimSpace(string(out)) != "UTF-8" {
			t.Fatalf("charmap %q lang %q", out, lang)
		}
		if lang != "" && env["LANG"] != lang {
			t.Fatalf("LANG %q", env["LANG"])
		}
	}
}

// Resolver output for the active home has a shell, the requested cwd and the terminal variables
func assertHostTerminalResolverUsesRealHost(t *testing.T, tmp string) {
	t.Helper()
	argv, cwd, env, shellName, err := resolveHostTerminalArgv(t.Context(), home.Dir(t.Context()), tmp)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := pypath.Resolve(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 || argv[0] == "" || cwd != resolved || shellName == "" {
		t.Fatalf("argv %v cwd %q shell %q", argv, cwd, shellName)
	}
	if env["TERM"] != "xterm-256color" || env["COLORTERM"] != "truecolor" || env["ICEBATH_DESKTOP_TERMINAL"] != "1" {
		t.Fatalf("env TERM %q COLORTERM %q DESKTOP %q", env["TERM"], env["COLORTERM"], env["ICEBATH_DESKTOP_TERMINAL"])
	}
}

// test_web_host_terminal.py test_host_terminal_policy_rejects_dashboard_and_unauthenticated_public_bind
func TestHostTerminalPolicyRejectsDashboardAndUnauthenticatedPublicBind(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	app := NewFastAPI("Icebath Agent", "0.0.0", nil, nil)
	app.State.Set("ui_surface", "dashboard")
	app.State.Set("bound_host", "127.0.0.1")
	app.State.Set("auth_required", false)
	if hostTerminalRequestAllowed(app) {
		t.Fatal("dashboard surface allowed a host terminal")
	}
	app.State.Set("ui_surface", "webapp")
	app.State.Set("bound_host", "0.0.0.0")
	if hostTerminalRequestAllowed(app) {
		t.Fatal("unauthenticated public bind allowed a host terminal")
	}
	app.State.Set("auth_required", true)
	if !hostTerminalRequestAllowed(app) {
		t.Fatal("authenticated webapp refused a host terminal")
	}
}

// test_web_host_terminal.py test_host_terminal_resolver_uses_real_linux_shell
func TestHostTerminalResolverUsesRealLinuxShell(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	assertHostTerminalResolverUsesRealHost(t, t.TempDir())
}

// test_web_host_terminal.py test_host_terminal_relative_shell_never_reaches_argv
func TestHostTerminalRelativeShellNeverReachesArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tmp := t.TempDir()
	serverCwd := filepath.Join(tmp, "server")
	workspace := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(filepath.Join(serverCwd, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(serverCwd, "bin", "sh")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(serverCwd)
	t.Setenv("SHELL", "bin/sh")
	argv, cwd, _, _, err := resolveHostTerminalArgv(t.Context(), home.Dir(t.Context()), workspace)
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorkspace, _ := pypath.Resolve(workspace)
	if cwd != resolvedWorkspace || !filepath.IsAbs(argv[0]) {
		t.Fatalf("cwd %q argv %v", cwd, argv)
	}
	resolvedArgv, _ := pypath.Resolve(argv[0])
	resolvedPlanted, _ := pypath.Resolve(planted)
	if resolvedArgv == resolvedPlanted {
		t.Fatalf("planted relative shell reached argv %v", argv)
	}
}

// test_web_host_terminal.py test_host_terminal_resolver_applies_selected_profile_home_and_config
func TestHostTerminalResolverAppliesSelectedProfileHomeAndConfig(t *testing.T) {
	tmp := t.TempDir()
	realHome := filepath.Join(tmp, "real-home")
	current := filepath.Join(tmp, "profiles", "current")
	target := filepath.Join(tmp, "profiles", "target")
	for _, p := range []string{realHome, filepath.Join(current, "home"), filepath.Join(target, "home")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "config.yaml"), []byte("terminal:\n  home_mode: profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(current, "home"))
	t.Setenv("ICEBATH_HOME", current)
	t.Setenv("ICEBATH_REAL_HOME", realHome)
	t.Setenv("TERMINAL_HOME_MODE", "profile")
	t.Setenv("OPENAI_API_KEY", "current-profile-secret")
	_, _, env, _, err := resolveHostTerminalArgv(t.Context(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	if env["ICEBATH_HOME"] != target || env["HOME"] != filepath.Join(target, "home") || env["TERMINAL_HOME_MODE"] != "profile" {
		t.Fatalf("ICEBATH_HOME %q HOME %q TERMINAL_HOME_MODE %q", env["ICEBATH_HOME"], env["HOME"], env["TERMINAL_HOME_MODE"])
	}
	if _, leaked := env["OPENAI_API_KEY"]; leaked {
		t.Fatal("the current profile's secret reached the target profile shell")
	}
}
