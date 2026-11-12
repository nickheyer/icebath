package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// Product checkout the launcher builds from with a fixed launch env and no desktop entry
func desktopSource(t *testing.T) (string, *[]string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	root, acquired := tniSourceProducts(t)
	swapGUISteps(t)
	gui.projectRoot = func() string { return root }
	gui.launchEnv = func(context.Context, map[string]any) (map[string]string, []string, error) {
		return map[string]string{}, nil, nil
	}
	gui.registerEntry = func(io.Writer, bool) *deferredDesktopEntryInstall { return nil }
	return root, acquired
}

// Runs the launcher with exactly the namespace attributes given, the exit code and output returned
func desktopSourceGUI(t *testing.T, ns map[string]any) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdGUI(context.Background(), ns, "", &out)
	if err != nil {
		t.Fatalf("CmdGUI error %v, output %s", err, out.String())
	}
	return code, out.String()
}

func TestDesktopBuildOnlyPreparesOnceAndKeepsFreshLaunchFast(t *testing.T) {
	root, acquired := desktopSource(t)
	if code, out := desktopSourceGUI(t, map[string]any{"build_only": true}); code != 0 {
		t.Fatalf("code %d out %s", code, out)
	}
	app, err := os.ReadFile(filepath.Join(root, "apps/desktop/release/linux-unpacked/icebath"))
	if err != nil || string(app) != "desktop" {
		t.Fatalf("app %q %v", app, err)
	}
	if steps := tniSteps(t, root); !slices.Equal(steps, []string{"deps", "icons", "desktop"}) {
		t.Fatalf("steps %v", steps)
	}
	if !slices.Equal(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	for _, ws := range []string{"ui-tui", "web", "apps-desktop"} {
		if _, err := os.Stat(filepath.Join(root, "node_modules", ws)); err != nil {
			t.Fatalf("node_modules/%s %v", ws, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules/unrelated")); err == nil {
		t.Fatal("unrelated workspace prepared")
	}
	if code, out := desktopSourceGUI(t, map[string]any{"build_only": true}); code != 0 {
		t.Fatalf("second code %d out %s", code, out)
	}
	if !slices.Equal(*acquired, []string{"npm"}) {
		t.Fatalf("second acquired %v", *acquired)
	}
	if events := tniEvents(t, root); len(events) != 3 {
		t.Fatalf("second events %v", events)
	}
}

func TestFailedPackExitsWithoutLaunchingOrReplacingTheApp(t *testing.T) {
	root, acquired := desktopSource(t)
	app := filepath.Join(root, "apps/desktop/release/linux-unpacked/icebath")
	tniWrite(t, app, "previous app")
	tniWrite(t, filepath.Join(root, "fail-desktop"), "")
	launched := false
	gui.run = func(guiRun) (*guiResult, error) { launched = true; return &guiResult{}, nil }
	if code, out := desktopSourceGUI(t, map[string]any{"build_only": true}); code == 0 {
		t.Fatalf("code %d out %s", code, out)
	}
	if launched {
		t.Fatal("failed pack launched the app")
	}
	if raw, err := os.ReadFile(app); err != nil || string(raw) != "previous app" {
		t.Fatalf("app %q %v", raw, err)
	}
	if !slices.Equal(*acquired, []string{"npm"}) {
		t.Fatalf("acquired %v", *acquired)
	}
	if steps := tniSteps(t, root); !slices.Equal(steps, []string{"deps", "icons", "desktop"}) {
		t.Fatalf("steps %v", steps)
	}
	if staged, _ := filepath.Glob(filepath.Join(root, "apps/desktop", ".staging-*")); len(staged) != 0 {
		t.Fatalf("staging left %v", staged)
	}
}

func TestSkipBuildSourceChecksArtifactsWithoutProvisioning(t *testing.T) {
	root, acquired := desktopSource(t)
	ns := map[string]any{"source": true, "skip_build": true, "build_only": true}
	if code, out := desktopSourceGUI(t, ns); code != 1 {
		t.Fatalf("code %d out %s", code, out)
	}
	if len(*acquired) != 0 {
		t.Fatalf("acquired %v", *acquired)
	}
	tniWrite(t, filepath.Join(root, "apps/desktop/dist/index.html"), "prepared renderer")
	tniWrite(t, filepath.Join(root, "node_modules/electron/package.json"), "{}")
	if code, out := desktopSourceGUI(t, ns); code != 0 {
		t.Fatalf("second code %d out %s", code, out)
	}
	if len(*acquired) != 0 {
		t.Fatalf("second acquired %v", *acquired)
	}
}
