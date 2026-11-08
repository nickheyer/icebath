//go:build !windows

package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Discovery as production wires it, kept before any test replaces it
var (
	unpatchedFindCLI        = findCLI
	unpatchedHarnessSiteDir = harnessSiteDir
)

func resetHarnessProbe(t *testing.T) {
	t.Helper()
	clear := func() {
		harnessProbeCache.Lock()
		harnessProbeCache.ok = false
		harnessProbeCache.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestHarnessRunsOnAnInterpreterWithoutItsOwnSitePackages(t *testing.T) {
	python := harnessPython(t)
	f := newExecFix(t, nil)
	resetHarnessProbe(t)
	pin(t, &harnessRuntime, func(context.Context) (string, []string, string, error) {
		return t.TempDir(), []string{python, "-I"}, "", nil
	})
	pin(t, &findCLI, unpatchedFindCLI)
	pin(t, &harnessSiteDir, unpatchedHarnessSiteDir)
	t.Setenv("PYTHONPATH", "/wrong-abi")
	t.Setenv("PYTHONHOME", "/wrong-python")
	cmd := findCLI(f.env)
	if !slices.Equal(cmd, []string{python, "-m", "browser_harness.run"}) {
		t.Fatalf("cmd = %v", cmd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], "-S", cmd[1], cmd[2], "--version")
	c.Env = envList(baseSubprocessEnv(context.Background(), f.env))
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(string(out)); got != "0.1.13" {
		t.Fatalf("version = %q", got)
	}
}

func TestBrowserExecChildEnvironment(t *testing.T) {
	f := newExecFix(t, nil)
	probe := filepath.Join(f.dir, "probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf 'stdin=%s\\n' \"$(cat)\"\nawk 'BEGIN { for (k in ENVIRON) print k \"\\t\" ENVIRON[k] }'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pin(t, &findCLI, func(*tools.Env) []string { return []string{probe} })
	pin(t, &harnessSiteDir, func() string { return "/harness-site" })
	pin(t, &execUsingLightpanda, func(*tools.Env) bool { return false })
	pin(t, &execSessionInfo, func(_ *tools.Env, key string) (map[string]any, error) {
		if key != "bu-named-research" {
			t.Errorf("cache key = %q", key)
		}
		return map[string]any{"cdp_url": "ws://127.0.0.1:47000/private", "features": map[string]any{"local": true}}, nil
	})
	t.Setenv("PYTHONPATH", "/wrong-abi")
	t.Setenv("PYTHONHOME", "/wrong-python")
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	t.Setenv("BROWSERBASE_API_KEY", "browser-key")
	t.Setenv("BROWSER_USE_API_KEY", "bu-key")
	t.Setenv("KEEP_BROWSER_PROBE", "kept")
	t.Setenv("ANONYMIZED_TELEMETRY", "")
	os.Unsetenv("ANONYMIZED_TELEMETRY")
	result := f.run(t, "print('payload')", "research", "owner")
	if result["success"] != true {
		t.Fatalf("%v", result)
	}
	output := result["output"].(string)
	stdin, rest, _ := strings.Cut(output, "\n")
	if stdin != "stdin=print('payload')" {
		t.Fatalf("stdin line = %q", stdin)
	}
	child := map[string]string{}
	for _, line := range strings.Split(rest, "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			child[k] = v
		}
	}
	for _, key := range []string{"PYTHONHOME", "OPENAI_API_KEY", "BROWSERBASE_API_KEY", privateBrowserSentinel} {
		if _, ok := child[key]; ok {
			t.Errorf("%s reached the child", key)
		}
	}
	want := map[string]string{"PYTHONPATH": "/harness-site", "BU_NAME": "research", "BU_CDP_WS": "ws://127.0.0.1:47000/private",
		"ANONYMIZED_TELEMETRY": "false", "BROWSER_USE_API_KEY": "bu-key", "KEEP_BROWSER_PROBE": "kept"}
	for k, v := range want {
		if child[k] != v {
			t.Errorf("%s = %q, want %q", k, child[k], v)
		}
	}
	if !slices.Equal(f.attached, [][2]string{{"owner", "ws://127.0.0.1:47000/private"}}) {
		t.Fatalf("attached = %v", f.attached)
	}
}
