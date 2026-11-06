//go:build !windows

package browser

import (
	"path/filepath"
	"testing"
)

// The exported resolution honours the configured override only when allowed, as the launcher's does
func TestChromiumExecutableExport(t *testing.T) {
	root, _ := runtimeHomes(t)
	override := filepath.Join(root, "external browser")
	env := pinnedEnv(override)
	pin(t, &runtimePMExecutable, func() string { return "/pm/chromium" })
	if got := ChromiumExecutable(env, true); got != override {
		t.Fatalf("resolved %q", got)
	}
	if got := ChromiumExecutable(env, false); got != "/pm/chromium" {
		t.Fatalf("resolved %q", got)
	}
}

// The exported launch starts the fake through the launcher and the exported stop ends it and forgets the key
func TestLaunchChromiumAndStopBrowserExports(t *testing.T) {
	env := launcherTestEnv(t)
	exe := fakeExe(t, "serving-chromium")
	b, err := LaunchChromium(env, "meet", LaunchOptions{Executable: exe, Ephemeral: true, Headed: boolp(false), ProcEnv: environ()})
	if err != "" || b == nil || b.PID == nil {
		t.Fatalf("launch %q", err)
	}
	if getBrowser("meet") != b || !launcherPidAlive(int(*b.PID)) {
		t.Fatal("launched browser not live and registered")
	}
	StopBrowser("meet")
	gone(t, *b.PID)
	if getBrowser("meet") != nil {
		t.Fatal("stopped browser still registered")
	}
}
