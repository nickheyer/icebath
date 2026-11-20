package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// homes fixture, a temp home and a baked path file under it, pm holding no chromium until a test publishes one
func runtimeHomes(t *testing.T) (string, *string) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "home"))
	os.Mkdir(filepath.Join(root, "home"), 0o755)
	pin(t, &bakedExecutableFile, filepath.Join(root, "etc", "chromium-executable-path"))
	published := new(string)
	pin(t, &runtimePMExecutable, func() string { return *published })
	pin(t, &runtimeIsTermux, func() bool { return false })
	pin(t, &runtimeCurrentTarget, func() (string, error) { return "linux-x64", nil })
	return root, published
}

func fakePMChromium(t *testing.T, store string, published *string) string {
	binary := filepath.Join(store, "chromium", "browser", "chrome")
	os.MkdirAll(filepath.Dir(binary), 0o755)
	os.WriteFile(binary, []byte("browser-fixture"), 0o755)
	*published = binary
	return binary
}

func pinnedEnv(path string) *tools.Env {
	return &tools.Env{Config: config.Tree{"browser": map[string]any{"executable_path": path}}}
}

func TestChromiumResolvesInstalledBinaryWithoutMutation(t *testing.T) {
	root, published := runtimeHomes(t)
	binary := fakePMChromium(t, filepath.Join(root, "tools"), published)
	before := os.Environ()
	if got := chromiumExecutable(&tools.Env{}, true); got != binary {
		t.Fatalf("resolved %q", got)
	}
	if strings.Join(os.Environ(), "\n") != strings.Join(before, "\n") {
		t.Fatal("resolution changed the environment")
	}
}

func TestConfiguredExecutableWinsWithoutInstalling(t *testing.T) {
	root, _ := runtimeHomes(t)
	override := filepath.Join(root, "external browser")
	env := pinnedEnv(override)
	pin(t, &runtimePMExecutable, func() string { t.Fatal("browser resolution must not read pm for a pinned browser"); return "" })
	if got := chromiumExecutable(env, true); got != override {
		t.Fatalf("resolved %q", got)
	}
	pin(t, &runtimePMExecutable, func() string { return "" })
	if got := chromiumExecutable(env, false); got != "" {
		t.Fatalf("resolved %q", got)
	}
	if lexists(filepath.Join(root, "tools")) {
		t.Fatal("resolution provisioned a tool store")
	}
}

func TestAgentBrowserEnvIsNotAnOverride(t *testing.T) {
	root, _ := runtimeHomes(t)
	t.Setenv("AGENT_BROWSER_EXECUTABLE_PATH", filepath.Join(root, "legacy"))
	if got := chromiumExecutable(&tools.Env{}, true); got != "" {
		t.Fatalf("resolved %q", got)
	}
}

func TestOverrideIsReadFromTheActiveProfile(t *testing.T) {
	runtimeHomes(t)
	a, b := pinnedEnv("/opt/a/chrome"), pinnedEnv("")
	for _, c := range []struct {
		env  *tools.Env
		want string
	}{{a, "/opt/a/chrome"}, {b, ""}, {a, "/opt/a/chrome"}} {
		if got := chromiumExecutable(c.env, true); got != c.want {
			t.Fatalf("resolved %q want %q", got, c.want)
		}
	}
}

func TestBakedDockerPathAfterPM(t *testing.T) {
	root, published := runtimeHomes(t)
	baked := filepath.Join(root, "opt", "chrome")
	os.MkdirAll(filepath.Dir(baked), 0o755)
	os.WriteFile(baked, []byte("x"), 0o644)
	os.MkdirAll(filepath.Dir(bakedExecutableFile), 0o755)
	os.WriteFile(bakedExecutableFile, []byte(baked+"\n"), 0o644)
	if got := chromiumExecutable(&tools.Env{}, true); got != baked {
		t.Fatalf("resolved %q", got)
	}
	binary := fakePMChromium(t, filepath.Join(root, "tools"), published)
	if got := chromiumExecutable(&tools.Env{}, true); got != binary {
		t.Fatalf("resolved %q", got)
	}
}

func TestBakedPathToAMissingBinaryIsIgnoredAndHinted(t *testing.T) {
	root, _ := runtimeHomes(t)
	os.MkdirAll(filepath.Dir(bakedExecutableFile), 0o755)
	os.WriteFile(bakedExecutableFile, []byte(filepath.Join(root, "gone")), 0o644)
	if got := chromiumExecutable(&tools.Env{}, true); got != "" {
		t.Fatalf("resolved %q", got)
	}
	if hint := chromiumMissingHint(&tools.Env{}); !strings.Contains(hint, "pull the current Icebath image") {
		t.Fatalf("hint %q", hint)
	}
}

func TestSystemChromiumOnlyWherePMBuildsNone(t *testing.T) {
	root, _ := runtimeHomes(t)
	bindir := filepath.Join(root, "bin")
	os.Mkdir(bindir, 0o755)
	exe := filepath.Join(bindir, "chromium-browser")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bindir)
	if got := chromiumExecutable(&tools.Env{}, true); got != "" {
		t.Fatalf("resolved %q", got)
	}
	if hint := chromiumMissingHint(&tools.Env{}); hint != "Chromium is not installed: run `icebath pm install chromium`." {
		t.Fatalf("hint %q", hint)
	}
	pin(t, &runtimeCurrentTarget, func() (string, error) { return "linux-arm64-bionic", nil })
	if got := chromiumExecutable(&tools.Env{}, true); got != exe {
		t.Fatalf("resolved %q", got)
	}
}

func TestTermuxHintNeverMentionsNpm(t *testing.T) {
	runtimeHomes(t)
	pin(t, &runtimeIsTermux, func() bool { return true })
	if hint := chromiumMissingHint(&tools.Env{}); !strings.Contains(hint, "pkg install") || strings.Contains(hint, "npm") || strings.Contains(hint, "npx") {
		t.Fatalf("hint %q", hint)
	}
}
