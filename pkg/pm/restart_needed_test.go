package pm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Temp install whose core locks offline against local wheels, with a home enabling nothing
func pubPluginWorld(t *testing.T) (string, string) {
	t.Helper()
	tmp := ieClientEnv(t)
	homeDir := filepath.Join(tmp, ".icebath")
	core := filepath.Join(tmp, "core")
	wheels := filepath.Join(tmp, "wheels")
	uv, python := peUV(t), peHostPython(t)
	for _, key := range []string{"ICEBATH_MANAGED_MODE", "ICEBATH_INSTALL_ROOT", "VIRTUAL_ENV"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	for key, value := range map[string]string{"ICEBATH_RUNTIME_DIR": filepath.Join(tmp, "store"), "UV_CACHE_DIR": filepath.Join(tmp, "cache"),
		"XDG_CONFIG_HOME": filepath.Join(tmp, "config"), "XDG_CONFIG_DIRS": filepath.Join(tmp, "config"), EnvDisableLazyInstalls: "1", "UV_OFFLINE": "1"} {
		t.Setenv(key, value)
	}
	peWheel(t, wheels, "plugin_core_dep", "", nil, nil)
	peWheel(t, wheels, "plugin_proof_other", "", nil, nil)
	for _, pin := range []string{"1.0", "2.0"} {
		peWheel(t, wheels, "plugin_proof_dep", pin, nil, nil)
	}
	peWrite(t, filepath.Join(homeDir, "config.yaml"), "plugins:\n  enabled: []\n  disabled: []\n")
	links, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(core, "pyproject.toml"), "[project]\nname=\"plugin-proof-core\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"plugin-core-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(links)+"]\n")
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": tmp, "UV_CACHE_DIR": filepath.Join(tmp, "cache"), "UV_OFFLINE": "1"}
	peRun(t, []string{uv, "lock", "--python", python}, core, env)
	peSwap(t, &repoRoot, func() string { return core })
	peToolchain(t, uv, python)
	wsHost(t, "")
	return core, homeDir
}

// Enables exactly these plugins with their declared requirements and returns the generation the sync selects
func pubPublishPlugins(t *testing.T, core, homeDir string, plugins map[string][]string) string {
	t.Helper()
	names := []string{}
	for name, requirements := range plugins {
		names = append(names, name)
		plugin := filepath.Join(homeDir, "plugins", name)
		if isDirectory(plugin) {
			continue
		}
		manifest, _ := yaml.Marshal(map[string]any{"name": name, "version": "1.0", "python_dependencies": requirements})
		peWrite(t, filepath.Join(plugin, "plugin.yaml"), string(manifest))
		body := ""
		for _, spec := range requirements {
			module := strings.NewReplacer("-", "_").Replace(strings.FieldsFunc(spec, func(r rune) bool { return r == '=' || r == '<' || r == '>' })[0])
			body += "import " + module + "\n"
		}
		peWrite(t, filepath.Join(plugin, "__init__.py"), body+"from pathlib import Path\nPath(__file__).with_name('imported').touch()\ndef register(ctx):\n    pass\n")
	}
	sort.Strings(names)
	config, _ := yaml.Marshal(map[string]any{"plugins": map[string]any{"enabled": names, "disabled": []string{}}})
	peWrite(t, filepath.Join(homeDir, "config.yaml"), string(config))
	if err := SyncVenv(context.Background(), SyncVenvOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	return svSelected(t, core)
}

// Makes this process import from a generation the way a process booted on it does
func pubBoot(t *testing.T, environment string) {
	t.Helper()
	site, err := SitePackages(environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", VenvBinDir(environment, runtime.GOOS == "windows")+string(os.PathListSeparator)+"/usr/bin:/bin")
	importPath.Lock()
	importPath.entries = append([]string{site}, importPath.entries...)
	importPath.Unlock()
}

func TestRestartNeededOnASupersededGenerationUntilItAdopts(t *testing.T) {
	core, homeDir := pubPluginWorld(t)
	svKeepProcess(t)
	ctx := context.Background()
	first := pubPublishPlugins(t, core, homeDir, map[string][]string{"base": {"plugin-proof-dep==1.0"}})
	if reason, err := RestartNeeded(ctx, core); reason != "" || err != nil {
		t.Fatalf("a process that never booted from the install needs no restart: %q %v", reason, err)
	}
	if ok, err := AdoptSelected(ctx, core); !ok || err != nil {
		t.Fatalf("adopt %v %v", ok, err)
	}
	pubBoot(t, first)
	if reason, err := RestartNeeded(ctx, core); reason != "" || err != nil {
		t.Fatalf("booted on the selection %q %v", reason, err)
	}
	second := pubPublishPlugins(t, core, homeDir, map[string][]string{"base": {"plugin-proof-dep==1.0"}, "adds": {"plugin-proof-other"}})
	if second == first {
		t.Fatal("adding a plugin selected no new generation")
	}
	reason, err := RestartNeeded(ctx, core)
	if err != nil || reason == "" || !strings.Contains(reason, filepath.Base(filepath.Dir(second))) {
		t.Fatalf("reason %q %v", reason, err)
	}
	if ok, err := AdoptSelected(ctx, core); !ok || err != nil {
		t.Fatalf("adopt %v %v", ok, err)
	}
	if reason, err := RestartNeeded(ctx, core); reason != "" || err != nil {
		t.Fatalf("adopted process still owes %q %v", reason, err)
	}
}
