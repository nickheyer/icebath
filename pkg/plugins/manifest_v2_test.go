package plugins

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type mv2Plugin struct{ dir, manifest, init string }

// Plugin whose manifest carries the name version and description lines plus extra and whose register runs body
func mv2Dir(name, extra, body string) mv2Plugin {
	return mv2Plugin{dir: name, manifest: "name: " + name + "\nversion: 0.1.0\ndescription: test " + name + "\n" + extra,
		init: "def register(ctx):\n    " + body + "\n"}
}

// Log capture of the plugins logger at the given level, the caplog at_level twin
func mv2Logs(t *testing.T, level slog.Level) *bytes.Buffer {
	var buf bytes.Buffer
	saved := Logger
	Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &buf}, &slog.HandlerOptions{Level: level}))
	t.Cleanup(func() { Logger = saved })
	return &buf
}

// Discovers host isolated bundled and user directory plugins of a temp home under the plugins section lines
func mv2Discover(t *testing.T, bundled, user []mv2Plugin, pluginsYAML string) (*Manager, string) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, "icebath_home")
	bundledRoot := filepath.Join(root, "bundled")
	for base, list := range map[string][]mv2Plugin{bundledRoot: bundled, filepath.Join(h, "plugins"): user} {
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			mv2Write(t, filepath.Join(base, p.dir, "plugin.yaml"), p.manifest)
			mv2Write(t, filepath.Join(base, p.dir, "__init__.py"), p.init)
		}
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: "+IsolationHost+"\n"+pluginsYAML)
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "0")
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundledRoot)
	python := liveInterpreters(t)["system"]
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m := NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	m.DiscoverAndLoad(homeCtx(h), false)
	return m, h
}

// Python statement appending the repr of expr to a probe file
func mv2Probe(probe, expr string) string {
	return "open(" + pyQuote(probe) + ", 'a').write(repr(" + expr + ") + '\\n')"
}

// File write that creates the parent directories first
func mv2Write(t *testing.T, path, body string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body)
}

func readProbe(t *testing.T, probe string) []string {
	raw, err := os.ReadFile(probe)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func TestV1UnknownFieldsDoNotWarnLoudly(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("oldie", "mystery_field: true\n", "pass")}, "  enabled: [oldie]\n")
	if p := m.plugins["oldie"]; p == nil || !p.Enabled || strings.Contains(logs.String(), "mystery_field") {
		t.Fatalf("oldie = %+v logs %s", p, logs)
	}
}

func TestManifestV2Parsing(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	t.Run("v2_fields_parse", func(t *testing.T) {
		extra := "manifest_version: 2\napi_version: 1\nlicense: MIT\nhomepage: https://example.com/modern\ntags: [gateway, demo]\n" +
			"requires_plugins:\n- id: other\n  version_range: '>=1.0,<2'\n- bare-dep\npython_dependencies: ['requests>=2.0,<3']\n" +
			"config_schema:\n  api_url: {type: str, default: '', description: x}\n"
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("modern", extra, "pass")}, "  enabled: [modern]\n")
		p := m.plugins["modern"]
		mf := p.Manifest
		if mf.ManifestVersion != 2 || mf.APIVersion == nil || *mf.APIVersion != 1 || mf.License != "MIT" || mf.Homepage != "https://example.com/modern" ||
			!reflect.DeepEqual(mf.Tags, []string{"gateway", "demo"}) || !reflect.DeepEqual(mf.PythonDeps, []string{"requests>=2.0,<3"}) {
			t.Fatalf("manifest = %+v", mf)
		}
		if want := []PluginDependency{{ID: "other", VersionRange: ">=1.0,<2"}, {ID: "bare-dep"}}; !reflect.DeepEqual(mf.RequiresPlugins, want) {
			t.Fatalf("requires_plugins = %+v", mf.RequiresPlugins)
		}
		if _, ok := mf.ConfigSchema["api_url"]; !ok || !(p.Enabled || p.Error != "") {
			t.Fatalf("schema %v enabled %v error %q", mf.ConfigSchema, p.Enabled, p.Error)
		}
	})
	t.Run("unknown_field_in_v2_warns_but_loads", func(t *testing.T) {
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("modern", "manifest_version: 2\nhovercraft: eels\n", "pass")}, "  enabled: [modern]\n")
		if !m.plugins["modern"].Enabled || !strings.Contains(logs.String(), "hovercraft") {
			t.Fatalf("logs %s", logs)
		}
	})
	t.Run("future_manifest_version_warns_but_loads", func(t *testing.T) {
		future := fmt.Sprint(SupportedManifestVersion + 5)
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("fromfuture", "manifest_version: "+future+"\n", "pass")}, "  enabled: [fromfuture]\n")
		if !m.plugins["fromfuture"].Enabled || !strings.Contains(logs.String(), future) {
			t.Fatalf("logs %s", logs)
		}
	})
	t.Run("malformed_v2_fields_warn_and_degrade", func(t *testing.T) {
		extra := "manifest_version: 2\napi_version: banana\nrequires_plugins: not-a-list\npython_dependencies: {nope: 1}\ntags: not-a-list\n"
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("sloppy", extra, "pass")}, "  enabled: [sloppy]\n")
		p := m.plugins["sloppy"]
		if !p.Enabled || p.Manifest.APIVersion != nil || len(p.Manifest.RequiresPlugins) != 0 || len(p.Manifest.PythonDeps) != 0 || len(p.Manifest.Tags) != 0 {
			t.Fatalf("sloppy = %+v manifest %+v", p, p.Manifest)
		}
	})
}

func TestManifestV2ConfigSchema(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	t.Run("type_mismatch_warns_but_loads", func(t *testing.T) {
		extra := "manifest_version: 2\nconfig_schema:\n  api_url: {type: str}\n  retries: {type: int}\n"
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("cfgd", extra, "pass")},
			"  enabled: [cfgd]\n  entries:\n    cfgd:\n      settings: {api_url: 42, retries: 3}\n")
		text := logs.String()
		parts := strings.Split(text, "should be")
		if !m.plugins["cfgd"].Enabled || !strings.Contains(text, "plugins.entries.cfgd.settings.api_url") || !strings.Contains(text, "should be str") ||
			strings.Contains(parts[len(parts)-1], "retries") {
			t.Fatalf("logs %s", text)
		}
	})
	t.Run("required_key_missing_warns", func(t *testing.T) {
		w := ValidateConfigSchema("p", map[string]map[string]any{"token": {"type": "str", "required": true}}, map[string]any{})
		if len(w) == 0 || !strings.Contains(w[0], "required") || !strings.Contains(w[0], "plugins.entries.p.settings.token") {
			t.Fatalf("warnings = %v", w)
		}
	})
	t.Run("valid_settings_produce_no_warnings", func(t *testing.T) {
		schema := map[string]map[string]any{"api_url": {"type": "str"}, "retries": {"type": "int"}, "ratio": {"type": "float"}, "flag": {"type": "bool"}}
		if w := ValidateConfigSchema("p", schema, map[string]any{"api_url": "x", "retries": 2, "ratio": 0.5, "flag": true}); len(w) != 0 {
			t.Fatalf("warnings = %v", w)
		}
	})
	t.Run("bool_does_not_satisfy_int", func(t *testing.T) {
		w := ValidateConfigSchema("p", map[string]map[string]any{"retries": {"type": "int"}}, map[string]any{"retries": true})
		if len(w) == 0 || !strings.Contains(w[0], "should be int") {
			t.Fatalf("warnings = %v", w)
		}
	})
	t.Run("unknown_declared_type_skips_check", func(t *testing.T) {
		extra, start := "manifest_version: 2\nconfig_schema:\n  thing: {type: quaternion}\n", logs.Len()
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("weird", extra, "pass")},
			"  enabled: [weird]\n  entries:\n    weird:\n      settings: {thing: 1}\n")
		if text := logs.String()[start:]; !m.plugins["weird"].Enabled || !strings.Contains(text, "quaternion") || strings.Contains(text, "should be") {
			t.Fatalf("logs %s", logs)
		}
	})
}

// Fake pip pip3 and uv executables ahead of PATH that record every invocation
func mv2SpyInstallers(t *testing.T) string {
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls.txt")
	for _, name := range []string{"pip", "pip3", "uv"} {
		writeFile(t, filepath.Join(bin, name), "#!/bin/sh\necho \"$0 $*\" >> "+calls+"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestManifestV2PythonDependenciesSeam(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	t.Run("missing_pip_dep_surfaced_with_hint_not_installed", func(t *testing.T) {
		calls := mv2SpyInstallers(t)
		t.Setenv("PYTHONPATH", t.TempDir())
		extra := "manifest_version: 2\npython_dependencies: ['definitely-not-a-real-package-64165>=1.0,<2']\n"
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("pipful", extra, "pass")}, "  enabled: [pipful]\n")
		if !m.plugins["pipful"].Enabled || !strings.Contains(logs.String(), "definitely-not-a-real-package-64165") || !strings.Contains(logs.String(), "icebath pm repair") {
			t.Fatalf("logs %s", logs)
		}
		if raw, _ := os.ReadFile(calls); len(raw) != 0 {
			t.Fatalf("installer ran: %s", raw)
		}
	})
	t.Run("satisfied_pip_dep_is_quiet", func(t *testing.T) {
		t.Setenv("PYTHONPATH", writeEntryPointSite(t, "PyYAML", "6.0.2", "YAML parser", ""))
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("pipok", "manifest_version: 2\npython_dependencies: ['pyyaml>=5,<7']\n", "pass")}, "  enabled: [pipok]\n")
		if !m.plugins["pipok"].Enabled || strings.Contains(logs.String(), "pip install") || strings.Contains(logs.String(), "pyyaml") {
			t.Fatalf("logs %s", logs)
		}
	})
}

func TestCtxHasPluginHasPluginProbe(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe.txt")
	mv2Discover(t, nil, []mv2Plugin{mv2Dir("probe-target", "", "pass"),
		mv2Dir("prober", "manifest_version: 2\nrequires_plugins:\n- id: probe-target\n", mv2Probe(probe, "(ctx.has_plugin('probe-target'), ctx.has_plugin('nope'))"))},
		"  enabled: [probe-target, prober]\n")
	if got := strings.Join(readProbe(t, probe), " "); got != "(True, False)" {
		t.Fatalf("probe = %q", got)
	}
}

func TestRequiresIcebathGate(t *testing.T) {
	t.Run("gate_reads_the_running_code_version_not_dist_metadata", func(t *testing.T) {
		saved := RunningVersion
		RunningVersion = "0.21.4"
		t.Cleanup(func() { RunningVersion = saved })
		if RunningVersion != "0.21.4" || !VersionSatisfies(">=0.21.4", RunningVersion) {
			t.Fatalf("running = %q", RunningVersion)
		}
	})
	t.Run("prerelease_spellings_gate", func(t *testing.T) {
		for _, c := range []struct {
			spec, current string
			want          bool
		}{{">=99.0.0rc1", "0.21.4", false}, {">=0.23.0", "0.22.0rc1", false}, {">=0.21.0", "0.22.0rc1", true}, {">=1.2.3.post1", "1.2.3", true}, {"banana", "0.21.4", true}} {
			if got := VersionSatisfies(c.spec, c.current); got != c.want {
				t.Errorf("VersionSatisfies(%q, %q) = %v", c.spec, c.current, got)
			}
		}
	})
	t.Run("unsatisfied_requires_hermes_skips_without_importing", func(t *testing.T) {
		saved := RunningVersion
		RunningVersion = "1.2.3"
		t.Cleanup(func() { RunningVersion = saved })
		probe := filepath.Join(t.TempDir(), "probe.txt")
		m, _ := mv2Discover(t, nil, []mv2Plugin{mv2Dir("future", "requires_icebath: '>=99.0'\n", mv2Probe(probe, "'future'")),
			mv2Dir("current", "requires_icebath: '>=1.2,<2'\n", mv2Probe(probe, "'current'"))}, "  enabled: [future, current]\n")
		if got := readProbe(t, probe); !reflect.DeepEqual(got, []string{"'current'"}) || !strings.Contains(m.plugins["future"].Error, "requires icebath >=99.0") {
			t.Fatalf("probe %v future %+v", got, m.plugins["future"])
		}
	})
}

func TestImpostorDirCannotClaimABundledKey(t *testing.T) {
	logs := mv2Logs(t, slog.LevelInfo)
	probe := filepath.Join(t.TempDir(), "probe.txt")
	bundled := []mv2Plugin{mv2Dir("genuine", "", mv2Probe(probe, "'shadow=bundled'")), mv2Dir("overridable", "", mv2Probe(probe, "'override=bundled'"))}
	impostor := mv2Dir("impostor_dir", "", mv2Probe(probe, "'shadow=impostor'"))
	impostor.manifest = "name: genuine\nversion: 0.1.0\ndescription: impostor\n"
	m, _ := mv2Discover(t, bundled, []mv2Plugin{impostor, mv2Dir("overridable", "", mv2Probe(probe, "'override=user'"))}, "  enabled: [genuine, overridable]\n")
	got, text := strings.Join(readProbe(t, probe), " "), logs.String()
	if m.plugins["genuine"].Manifest.Source != "bundled" || !strings.Contains(got, "'shadow=bundled'") || strings.Contains(got, "impostor") {
		t.Fatalf("genuine %+v probe %s", m.plugins["genuine"].Manifest, got)
	}
	if m.plugins["overridable"].Manifest.Source != "user" || !strings.Contains(got, "'override=user'") || strings.Contains(got, "override=bundled") {
		t.Fatalf("overridable %+v probe %s", m.plugins["overridable"].Manifest, got)
	}
	if !strings.Contains(text, "impostor_dir") || !strings.Contains(text, "rename the directory") || !strings.Contains(text, "shadows the bundled copy") {
		t.Fatalf("logs %s", text)
	}
}

func TestListManifestIsRejectedWithAClearReasonAndHooksAlias(t *testing.T) {
	logs := mv2Logs(t, slog.LevelWarn)
	base := filepath.Join(t.TempDir(), "plugins")
	mv2Write(t, filepath.Join(base, "listy", "plugin.yaml"), "- name: listy\n")
	good := mv2Dir("hooky", "hooks: [pre_tool_call]\n", "pass")
	mv2Write(t, filepath.Join(base, "hooky", "plugin.yaml"), good.manifest)
	mv2Write(t, filepath.Join(base, "hooky", "__init__.py"), good.init)
	byName := map[string]*Manifest{}
	for _, mf := range ScanDirectory(base, "user", nil) {
		byName[mf.Name] = mf
	}
	if _, ok := byName["listy"]; ok || !strings.Contains(logs.String(), "top level must be a mapping") {
		t.Fatalf("manifests %v logs %s", byName, logs)
	}
	if h := byName["hooky"]; h == nil || !reflect.DeepEqual(h.ProvidesHooks, []string{"pre_tool_call"}) || h.Path != filepath.Join(base, "hooky") {
		t.Fatalf("hooky = %+v", h)
	}
}

func TestLoaderAndListingPreferTheInstalledDirectory(t *testing.T) {
	t.Setenv("PYTHONPATH", writeEntryPointSite(t, "twin", "9.9.9", "pip twin", "[icebath_agent.plugins]\ntwin = twin_pkg:register\n"))
	m, h := mv2Discover(t, nil, []mv2Plugin{mv2Dir("twin", "", "pass")}, "  enabled:\n  - twin\n")
	m.mu.RLock()
	loaded := m.plugins["twin"]
	m.mu.RUnlock()
	if loaded == nil || loaded.Manifest.Source != "user" {
		t.Fatalf("loaded %+v", loaded)
	}
	entries, err := discoverAllPlugins(homeCtx(h))
	if err != nil {
		t.Fatal(err)
	}
	var rows []PluginEntry
	for _, e := range entries {
		if e.Name == "twin" {
			rows = append(rows, e)
		}
	}
	if len(rows) != 1 || rows[0].Source != "user" || !strings.HasSuffix(rows[0].Dir, filepath.Join("plugins", "twin")) {
		t.Fatalf("rows %+v", rows)
	}
}
