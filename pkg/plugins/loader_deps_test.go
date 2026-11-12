package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

const depDist = "icebath-depprobe-fixture"

var depReq = depDist + ">=1.0,<2"

// Writes a dist-info directory for the fixture distribution at a version into a site directory
func writeDistInfo(t *testing.T, site, version string) {
	t.Helper()
	info := filepath.Join(site, strings.ReplaceAll(depDist, "-", "_")+"-"+version+".dist-info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Metadata-Version: 2.1\nName: " + depDist + "\nVersion: " + version + "\n"
	if err := os.WriteFile(filepath.Join(info, "METADATA"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Commits a PM selection for the running tree under a temp home the way a sync leaves it
func selectGeneration(t *testing.T, withDist bool, version string) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	venv := filepath.Join(pm.InstallStateDir(ctx, pm.RepoRoot()), "environments", "gen1", "venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /nonexistent\nversion = 3.12.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if withDist {
		writeDistInfo(t, site, version)
	}
	facts, _ := json.Marshal(map[string]any{"schema": 1, "packages": map[string]any{"venv": map[string]any{"environment": venv}}})
	path := pm.EnvironmentRuntimeFactsPath(ctx, pm.RepoRoot())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, facts, 0o644); err != nil {
		t.Fatal(err)
	}
	return site
}

func TestAnImportableDependencyIsNotReported(t *testing.T) {
	site := selectGeneration(t, true, "1.0")
	t.Setenv("PYTHONPATH", site)
	if got := missingPythonDependencies(context.Background(), &Manifest{Name: "depprobe", PythonDeps: []string{depReq}}); len(got) != 0 {
		t.Fatalf("missing = %v", got)
	}
}

func TestSelectedGenerationProbeIsFalseWithoutASelection(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if installedInSelectedEnvironment(context.Background(), []string{depReq}) {
		t.Fatal("selected generation reported the dependency without a selection")
	}
}

func TestAnInstalledVersionTheRequirementRejectsIsMissing(t *testing.T) {
	selectGeneration(t, true, "1.7")
	running := filepath.Join(t.TempDir(), "running-site")
	writeDistInfo(t, running, "1.0")
	t.Setenv("PYTHONPATH", running)
	raised := depDist + ">=1.5,<2"
	ctx := context.Background()
	if got := missingPythonDependencies(ctx, &Manifest{Name: "depprobe", PythonDeps: []string{raised}}); len(got) != 1 || got[0] != raised {
		t.Fatalf("missing = %v", got)
	}
	if !installedInSelectedEnvironment(ctx, []string{raised}) {
		t.Fatal("selected generation 1.7 should satisfy the raised floor")
	}
	if installedInSelectedEnvironment(ctx, []string{depDist + ">=1.8"}) {
		t.Fatal("selected generation 1.7 should not satisfy >=1.8")
	}
}

// Home whose selected generation runs the system Python and whose resident PM runtime imports its packaging
func markerRuntimeWorld(t *testing.T) {
	t.Helper()
	python, err := filepath.EvalSymlinks(liveInterpreters(t)["system"])
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-c", "import packaging, os; print(os.path.dirname(os.path.dirname(packaging.__file__)))").Output()
	if err != nil {
		t.Fatalf("judging a requirement marker needs the packaging distribution in python3: %v", err)
	}
	tmp := t.TempDir()
	t.Cleanup(pm.OverrideCoreRepo(mustMkdir(t, filepath.Join(tmp, "core"))))
	selectGeneration(t, false, "1.0")
	venv := filepath.Join(pm.InstallStateDir(context.Background(), pm.RepoRoot()), "environments", "gen1", "venv")
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), "home = "+filepath.Dir(python)+"\nversion = 3.12.4\n")
	mustMkdir(t, filepath.Dir(pm.VenvPython(venv)))
	if err := os.Symlink(python, pm.VenvPython(venv)); err != nil {
		t.Fatal(err)
	}
	runtimeDir := mustMkdir(t, filepath.Join(tmp, "pm-runtime"))
	record, _ := json.Marshal(map[string]any{"python": python, "sitePackages": strings.TrimSpace(string(out))})
	writeFile(t, filepath.Join(runtimeDir, "pm-runtime.json"), string(record))
	stamp, _ := json.Marshal(map[string]any{"distribution": "nix", "pmRuntime": runtimeDir})
	writeFile(t, pm.InstallStampPath(pm.RepoRoot()), string(stamp))
}

func TestADependencyExcludedByItsMarkerIsNeverMissing(t *testing.T) {
	markerRuntimeWorld(t)
	t.Setenv("PYTHONPATH", "")
	excluded := depDist + ">=1.0,<2; python_version < '3.0'"
	got := missingPythonDependencies(context.Background(), &Manifest{Name: "depprobe", PythonDeps: []string{excluded, depReq}})
	if len(got) != 1 || got[0] != depReq {
		t.Fatalf("missing = %v", got)
	}
}

func TestDiscoveryWarnsRestartOrRepair(t *testing.T) {
	for _, withDist := range []bool{true, false} {
		selectGeneration(t, withDist, "1.0")
		t.Setenv("PYTHONPATH", "")
		buf := captureLogs(t)
		NewManager(t.TempDir()).warnPythonDependencies(context.Background(), &Manifest{Name: "depprobe", Key: "depprobe", PythonDeps: []string{depReq}})
		out := buf.String()
		if withDist && (!strings.Contains(out, "installed after this process started") || !strings.Contains(out, "restart") || strings.Contains(out, "pm repair")) {
			t.Fatalf("restart remedy missing: %s", out)
		}
		if !withDist && (!strings.Contains(out, "not installed") || !strings.Contains(out, "pm repair")) {
			t.Fatalf("repair remedy missing: %s", out)
		}
	}
}

func TestDistNameAndLookup(t *testing.T) {
	for req, want := range map[string]string{"foo>=1": "foo", "foo[bar]; python_version>'3'": "foo", " foo ": "", "foo~=1.0": "foo", ">=1": ""} {
		if got := distName(req); got != want {
			t.Errorf("distName(%q) = %q, want %q", req, got, want)
		}
	}
	if distInstalled(">=1", []string{t.TempDir()}) != nil {
		t.Fatal("a requirement naming no distribution is unprobeable")
	}
	site := t.TempDir()
	writeDistInfo(t, site, "1.2rc1")
	if got := distInstalled("Icebath.Depprobe_Fixture>=1.1", []string{site}); got == nil || !*got {
		t.Fatal("normalized name with a prerelease inside the specifier should be installed")
	}
	if got := distInstalled(depDist+"<1", []string{site}); got == nil || *got {
		t.Fatal("version outside the specifier should be reported not installed")
	}
	if got := distInstalled(depDist+" @ https://example.invalid/x.whl", []string{site}); got == nil || !*got {
		t.Fatal("presence decides for a requirement without a specifier")
	}
}

func TestDeclaredRequirementsFromManifestWithoutDirectory(t *testing.T) {
	mf := &Manifest{Name: "x", PythonDeps: []string{"a>=1", "b"}}
	if got := declaredRequirements(mf); len(got) != 2 || got[0] != "a>=1" {
		t.Fatalf("declared = %v", got)
	}
	if !appliesHere(context.Background(), "a>=1") {
		t.Fatal("a requirement without a marker always applies")
	}
	if !appliesHere(context.Background(), "a>=1; ][") {
		t.Fatal("an unparseable requirement keeps being reported")
	}
}

func TestKeyboardInterruptFromCallerThreadHookPropagates(t *testing.T) {
	m := timeoutManager(t, "1.0")
	var mu sync.Mutex
	later := 0
	addHook(m, "subagent_stop", func(ctx context.Context, kw map[string]any) (any, error) { return nil, ErrUserInterrupt })
	addHook(m, "subagent_stop", func(ctx context.Context, kw map[string]any) (any, error) {
		mu.Lock()
		later++
		mu.Unlock()
		return nil, nil
	})
	buf := captureLogs(t)
	func() {
		defer func() {
			r := recover()
			u, ok := r.(*UserInterrupt)
			if !ok || !errors.Is(u, ErrUserInterrupt) {
				t.Fatalf("recovered %v, want a UserInterrupt", r)
			}
		}()
		m.InvokeHook(context.Background(), "subagent_stop", map[string]any{"parent_session_id": "p1"})
		t.Fatal("InvokeHook returned instead of raising the interrupt")
	}()
	mu.Lock()
	defer mu.Unlock()
	if later != 0 {
		t.Fatal("a callback after the interrupt ran")
	}
	if strings.Contains(buf.String(), "subagent_stop") {
		t.Fatalf("the interrupt was reported as a hook failure: %s", buf.String())
	}
}

func TestInterruptEscapesUnboundedHookMiddlewareAndSections(t *testing.T) {
	m := timeoutManager(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	addHook(m, "on_session_start", func(ctx context.Context, kw map[string]any) (any, error) { return nil, ctx.Err() })
	expectInterrupt(t, func() { m.InvokeHook(ctx, "on_session_start", nil) })
	expectInterrupt(t, func() { m.AInvokeHook(ctx, "on_session_start", nil) })
	m.mu.Lock()
	m.middleware["llm_request"] = []*middlewareEntry{{name: "mw", fn: func(map[string]any) (any, error) { panic(ErrUserInterrupt) }}}
	m.mu.Unlock()
	expectInterrupt(t, func() { m.InvokeMiddleware("llm_request", "request", map[string]any{"request": map[string]any{}}) })
	section := &SystemPromptSection{ID: "s", Plugin: "p", ContentFunc: func(map[string]any) (any, error) { return nil, ErrUserInterrupt }}
	expectInterrupt(t, func() { renderPromptSectionText(section, map[string]any{}) })
}

// Fails unless the function unwinds with a UserInterrupt panic
func expectInterrupt(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if _, ok := recover().(*UserInterrupt); !ok {
			t.Fatal("expected a UserInterrupt to escape")
		}
	}()
	fn()
}

func TestPluginDebugHandlerTeesToStderr(t *testing.T) {
	savedLogger, savedOut := Logger, debugStderr
	pluginsDebugState.Lock()
	savedState := pluginsDebugState.enabled
	savedInstalled := pluginsDebugState.installed
	pluginsDebugState.installed = false
	pluginsDebugState.Unlock()
	var out bytes.Buffer
	debugStderr = &syncWriter{w: &out}
	t.Cleanup(func() {
		Logger, debugStderr = savedLogger, savedOut
		pluginsDebugState.Lock()
		pluginsDebugState.enabled, pluginsDebugState.installed = savedState, savedInstalled
		pluginsDebugState.Unlock()
	})
	t.Setenv(EnvPluginsDebug, "")
	installPluginDebugHandler(true)
	if out.Len() != 0 || excInfo(errors.New("x")) != nil {
		t.Fatal("debug handler installed while ICEBATH_PLUGINS_DEBUG is off")
	}
	t.Setenv(EnvPluginsDebug, "1")
	installPluginDebugHandler(true)
	logWarn("Failed to load plugin 'x': boom")
	text := out.String()
	if !strings.Contains(text, "[plugins] DEBUG ICEBATH_PLUGINS_DEBUG=1") || !strings.Contains(text, "[plugins] WARNING Failed to load plugin 'x': boom") {
		t.Fatalf("stderr tee = %q", text)
	}
	if info := excInfo(&PluginPanicError{Value: "v", Stack: []byte("stack")}); len(info) != 2 || !strings.Contains(info[1].(string), "stack") {
		t.Fatalf("exc_info = %v", info)
	}
	installPluginDebugHandler(true)
	if strings.Count(out.String(), "verbose plugin discovery logging enabled") != 1 {
		t.Fatal("handler installed twice")
	}
}

func TestBundledPluginsDirHonoursOverride(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "/opt/bundled")
	if GetBundledPluginsDir() != "/opt/bundled" {
		t.Fatal("override ignored")
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	if GetBundledPluginsDir() != filepath.Join(pm.RepoRoot(), "plugins") {
		t.Fatalf("default = %s", GetBundledPluginsDir())
	}
}

func TestAppliesHereEvaluatesTheMarkerAgainstThePluginInterpreter(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	host, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	python := pm.VenvPython(filepath.Join(repo, "venv"))
	if err := os.MkdirAll(filepath.Dir(python), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host, python); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pm.PatchRepoRoot(repo))
	for req, want := range map[string]bool{
		"a>=1; python_version >= '0'":                    true,
		"a>=1; python_version < '0'":                     false,
		"a; extra == 'cli'":                              false,
		"a; python_version >= '0' and extra == 'cli'":    false,
		"a; python_version < '0' or os_name != 'riscos'": true,
		"a; platform_machine ~= 'x86'":                   true,
	} {
		if got := appliesHere(context.Background(), req); got != want {
			t.Errorf("%q applies %v, want %v", req, got, want)
		}
	}
}
