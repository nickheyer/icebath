package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Desktop checkout whose npm builder script runs the given pack body against the staging dir it is handed
func guiPackCheckout(t *testing.T, packBody string) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	tmp := t.TempDir()
	root := guiCheckout(t, tmp)
	desktopDir := filepath.Join(root, "apps", "desktop")
	writeTestFile(t, filepath.Join(desktopDir, "package.json"),
		`{"name":"desktop","version":"1.0.0","scripts":{"build":"node build.mjs","builder":"node pack.mjs"}}`, 0o644)
	writeTestFile(t, filepath.Join(desktopDir, "build.mjs"), "", 0o644)
	writeTestFile(t, filepath.Join(desktopDir, "pack.mjs"), "import { mkdirSync, writeFileSync } from 'node:fs';\n"+
		"import { join } from 'node:path';\n"+
		"const flag = '-c.directories.output=';\n"+
		"const staging = process.argv.find(arg => arg.startsWith(flag)).slice(flag.length);\n"+packBody, 0o644)
	liveExe := guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	t.Setenv("npm_config_cache", filepath.Join(tmp, "npm-cache"))
	t.Setenv("npm_config_update_notifier", "false")
	gui.sourceBuildEnv = func(_ context.Context, env map[string]string, _ bool) (map[string]string, error) {
		merged := map[string]string{}
		for _, kv := range os.Environ() {
			k, v, _ := strings.Cut(kv, "=")
			merged[k] = v
		}
		for k, v := range env {
			merged[k] = v
		}
		return merged, nil
	}
	gui.prepare = func(context.Context, string, []string, map[string]string, bool) error { return nil }
	gui.buildNeeded = func(context.Context, string, string, bool) bool { return true }
	gui.registerEntry = func(io.Writer, bool) *deferredDesktopEntryInstall { return nil }
	return desktopDir, liveExe
}

// Fails unless the live app holds want and no staging dir or previous tree is left behind
func assertGuiPackLeftovers(t *testing.T, desktopDir, liveExe, want string) {
	t.Helper()
	if raw, err := os.ReadFile(liveExe); err != nil || string(raw) != want {
		t.Fatalf("live exe %q %v, want %q", raw, err, want)
	}
	if staged, _ := filepath.Glob(filepath.Join(desktopDir, ".staging-*")); len(staged) != 0 {
		t.Fatalf("staging left %v", staged)
	}
	if previous, _ := filepath.Glob(filepath.Join(desktopDir, "release", "*.previous")); len(previous) != 0 {
		t.Fatalf("previous tree left %v", previous)
	}
}

func TestGuiFailedPackLeavesPreviousAppUntouched(t *testing.T) {
	desktopDir, liveExe := guiPackCheckout(t, "mkdirSync(join(staging, 'linux-unpacked', 'resources'), { recursive: true });\nprocess.exit(1);\n")
	writeTestFile(t, liveExe, "good build", 0o755)
	t.Setenv("ELECTRON_MIRROR", "https://example.test/electron/")
	if code, out := runCmdGUI(t, guiNS(map[string]any{"build_only": true}), ""); code != 1 {
		t.Fatalf("code %d out %s", code, out)
	}
	assertGuiPackLeftovers(t, desktopDir, liveExe, "good build")
}

func TestGuiSuccessfulPackSwapsNewAppIntoRelease(t *testing.T) {
	desktopDir, liveExe := guiPackCheckout(t, "const app = join(staging, 'linux-unpacked');\n"+
		"mkdirSync(app, { recursive: true });\n"+
		"writeFileSync(join(app, 'icebath'), 'new build');\n"+
		"writeFileSync(join(app, 'chrome-sandbox'), '');\n")
	writeTestFile(t, liveExe, "old build", 0o755)
	if code, out := runCmdGUI(t, guiNS(map[string]any{"build_only": true}), ""); code != 0 {
		t.Fatalf("code %d out %s", code, out)
	}
	assertGuiPackLeftovers(t, desktopDir, liveExe, "new build")
}

func TestGuiZeroExitPackWithoutArtifactKeepsPreviousApp(t *testing.T) {
	desktopDir, liveExe := guiPackCheckout(t, "mkdirSync(staging, { recursive: true });\n")
	writeTestFile(t, liveExe, "good build", 0o755)
	if code, out := runCmdGUI(t, guiNS(map[string]any{"build_only": true}), ""); code != 1 {
		t.Fatalf("code %d out %s", code, out)
	}
	assertGuiPackLeftovers(t, desktopDir, liveExe, "good build")
}
