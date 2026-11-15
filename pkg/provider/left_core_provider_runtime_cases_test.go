package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

const leftCoreAgentStartEnv = "ICEBATH_TEST_LEFT_CORE_AGENT_START"

// Runs a fresh process's agent start, printing every delivered notice, when the child env flag is set
func dispatchLeftCoreAgentStartEntry() {
	if os.Getenv(leftCoreAgentStartEnv) != "1" {
		return
	}
	RecoverLeftCoreAtStartup(context.Background(), func(message string) { fmt.Println("NOTICE", message) }, false)
	os.Exit(0)
}

// What a fresh process's agent start delivers for the bound process home
func agentStartInAnotherProcess(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), leftCoreAgentStartEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err, string(out))
	}
	var lines []string
	for _, line := range strings.SplitAfter(string(out), "\n") {
		if strings.HasPrefix(line, "NOTICE") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "")
}

// Fresh attempt memo, no terminal front end, and none of the copilot env names in the process environment
func leftCoreRuntimeFresh(t *testing.T) {
	t.Helper()
	leftCoreRowsFresh(t)
	prev := leftCoreTerminalFrontEnd.Load()
	leftCoreTerminalFrontEnd.Store(false)
	t.Cleanup(func() { leftCoreTerminalFrontEnd.Store(prev) })
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
}

// Home with the config bound as the process home, the lazy install policy left to its config
func copilotHome(t *testing.T, cfg string) (string, context.Context) {
	t.Helper()
	h := leftCoreHome(t, t.TempDir(), "home", "", cfg)
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	return h, context.Background()
}

// Catalog installs recorded as name and consent pairs, every one failing at the catalog repo
func failingInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(_ context.Context, name string, consent bool) (map[string]any, error) {
			*calls = append(*calls, fmt.Sprintf("%s:%v", name, consent))
			return map[string]any{"ok": false, "error": "fatal: unable to access the catalog repo"}, nil
		}
	})
	return calls
}

// Catalog installs that leave the plugin dir in the process home, recorded by name
func okInstalls(t *testing.T) *[]string {
	t.Helper()
	calls := &[]string{}
	withPluginHooks(t, func(hk *PluginManagerHooks) {
		hk.Install = func(ctx context.Context, name string, _ bool) (map[string]any, error) {
			*calls = append(*calls, name)
			mkPluginDir(t, home.Dir(ctx), name)
			return map[string]any{"ok": true}, nil
		}
	})
	return calls
}

// Resolves the runtime provider expecting the unknown provider auth error
func resolveExpectingAuthError(t *testing.T, ctx context.Context, h, requested string) {
	t.Helper()
	_, err := ResolveRuntimeProvider(ctx, h, requested, "", "", "")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("resolve error %T %v", err, err)
	}
}

// Text written to the process stderr while fn runs
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	fn()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
