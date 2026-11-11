package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Python preamble counting one import kind into a probe file the Go test reads back
func deferredProbePreamble(probe string) string {
	return "PROBE = " + strconv.Quote(probe) + "\n" +
		"def _bump(kind):\n" +
		"    with open(PROBE + '/' + kind, 'a') as f:\n" +
		"        f.write('x\\n')\n" +
		"def _count(kind):\n" +
		"    try:\n" +
		"        with open(PROBE + '/' + kind) as f:\n" +
		"            return len(f.read().splitlines())\n" +
		"    except FileNotFoundError:\n" +
		"        return 0\n"
}

// Probe of one synthetic platform plugin, its directory, manifest and import counters
type deferredProbe struct {
	dir, probe string
	mf         *Manifest
}

func (p *deferredProbe) count(kind string) int {
	raw, err := os.ReadFile(filepath.Join(p.probe, kind))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

func (p *deferredProbe) write(t *testing.T, name, body string) {
	t.Helper()
	writeFile(t, filepath.Join(p.dir, name), deferredProbePreamble(p.probe)+body)
}

// Bundled style platform plugin whose adapter import is deferred into register, the hermes _write_platform_plugin twin
func writeDeferredPlatform(t *testing.T, platform string, withTools bool, declares bool) *deferredProbe {
	t.Helper()
	root := t.TempDir()
	p := &deferredProbe{dir: filepath.Join(root, platform), probe: filepath.Join(root, "probe")}
	for _, d := range []string{p.dir, p.probe} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var provides []string
	manifest := "name: " + platform + "-platform\nkind: platform\nversion: 1.0.0\n"
	if declares {
		provides = []string{platform + "_call"}
		manifest += "provides_tools:\n- " + platform + "_call\n"
	}
	writeFile(t, filepath.Join(p.dir, "plugin.yaml"), manifest)
	p.write(t, "adapter.py", "_bump('adapter_imports')\n")
	init := "_bump('package_execs')\n\ndef register(ctx):\n    from . import adapter  # noqa: F401\n"
	if withTools {
		p.write(t, "tools.py", "_bump('tools_execs')\n\n"+
			"def _handler(**kwargs):\n    return 'ok'\n\n"+
			"def register_tools(ctx):\n"+
			"    ctx.register_tool(name='"+platform+"_call', toolset='"+platform+"', "+
			"schema={'type': 'function', 'function': {'name': '"+platform+"_call', 'description': 'call a peer', "+
			"'parameters': {'type': 'object', 'properties': {}}}}, handler=_handler, description='call a peer')\n")
		init += "    from .tools import register_tools\n    register_tools(ctx)\n"
	}
	p.write(t, "__init__.py", init)
	p.mf = &Manifest{Name: platform + "-platform", Kind: "platform", Source: "bundled", Path: p.dir, Key: platform + "-platform", ProvidesTools: provides}
	return p
}

// Live host home with a platform registry double, the manager loading nothing until a test registers a platform
func deferredWorld(t *testing.T) (string, *Manager, *ledgerPlatforms) {
	t.Helper()
	installLiveServices(t)
	platforms := &ledgerPlatforms{slots: map[[2]string]PlatformSlot{}}
	previous := installedServices()
	with := *previous
	with.Platforms = platforms
	InstallServices(&with)
	t.Cleanup(func() { InstallServices(previous) })
	h, m := liveHome(t, liveInterpreters(t)["system"], nil)
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n")
	return h, m, platforms
}

// Copy of the manager row of a plugin key read under the manager lock
func deferredRow(t *testing.T, m *Manager, key string) LoadedPlugin {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.plugins[key]
	if p == nil {
		t.Fatalf("plugin %s has no manager row", key)
	}
	return *p
}

func deferredPredeclared(m *Manager, key string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	got, ok := m.predeclaredTools[key]
	return got, ok
}

// Whether a WARN log line names every given text
func warnedWith(logs string, texts ...string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "level=WARN") {
			continue
		}
		hit := true
		for _, text := range texts {
			hit = hit && strings.Contains(line, text)
		}
		if hit {
			return true
		}
	}
	return false
}

func resolveProbe(h string) []string {
	return tools.Default.ResolveToolset(homeCtx(h), h, "probeplat")
}

// test_tools_module_registers_without_importing_the_adapter
func TestDeferredPlatformToolPreregistrationToolsModuleRegistersWithoutImportingTheAdapter(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	if got := resolveProbe(h); !reflect.DeepEqual(got, []string{"probeplat_call"}) {
		t.Fatalf("resolve_toolset = %v", got)
	}
	if p.count("adapter_imports") != 0 || p.count("tools_execs") != 1 || !deferredRow(t, m, "probeplat-platform").Deferred {
		t.Fatalf("adapter %d tools %d row %+v", p.count("adapter_imports"), p.count("tools_execs"), deferredRow(t, m, "probeplat-platform"))
	}
}

// test_plugin_without_tools_module_stays_fully_deferred
func TestDeferredPlatformToolPreregistrationPluginWithoutToolsModuleStaysFullyDeferred(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "barefoot", false, false)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	row := deferredRow(t, m, "barefoot-platform")
	if p.count("package_execs") != 0 || p.count("adapter_imports") != 0 || len(row.ToolsRegistered) != 0 {
		t.Fatalf("package %d adapter %d tools %v", p.count("package_execs"), p.count("adapter_imports"), row.ToolsRegistered)
	}
}

// test_tools_module_alone_does_not_opt_a_platform_in
func TestDeferredPlatformToolPreregistrationToolsModuleAloneDoesNotOptAPlatformIn(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "quietplat", true, false)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	row := deferredRow(t, m, "quietplat-platform")
	if p.count("package_execs") != 0 || p.count("tools_execs") != 0 || p.count("adapter_imports") != 0 || !row.Deferred || len(row.ToolsRegistered) != 0 {
		t.Fatalf("package %d tools %d adapter %d row %+v", p.count("package_execs"), p.count("tools_execs"), p.count("adapter_imports"), row)
	}
}

// test_package_body_runs_once_across_discovery_and_materialization
func TestDeferredPlatformToolPreregistrationPackageBodyRunsOnceAcrossDiscoveryAndMaterialization(t *testing.T) {
	h, m, platforms := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	if p.count("package_execs") != 1 {
		t.Fatalf("package body ran %d times at discovery", p.count("package_execs"))
	}
	platforms.get(homeCtx(h), "probeplat", h)
	if p.count("package_execs") != 1 || p.count("adapter_imports") != 1 {
		t.Fatalf("after materialization package %d adapter %d (load error %q)", p.count("package_execs"), p.count("adapter_imports"), loadedError(m, "probeplat-platform"))
	}
}

// test_tools_stay_attributed_after_materialization
func TestDeferredPlatformToolPreregistrationToolsStayAttributedAfterMaterialization(t *testing.T) {
	h, m, platforms := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	if got := deferredRow(t, m, "probeplat-platform").ToolsRegistered; !reflect.DeepEqual(got, []string{"probeplat_call"}) {
		t.Fatalf("pre-registered tools = %v", got)
	}
	platforms.get(homeCtx(h), "probeplat", h)
	row := deferredRow(t, m, "probeplat-platform")
	if !reflect.DeepEqual(row.ToolsRegistered, []string{"probeplat_call"}) || !row.Enabled {
		t.Fatalf("after materialization row %+v", row)
	}
}

// test_broken_tools_module_does_not_break_discovery
func TestDeferredPlatformToolPreregistrationBrokenToolsModuleDoesNotBreakDiscovery(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	writeFile(t, filepath.Join(p.dir, "tools.py"), "raise RuntimeError('boom')\n")
	logs := captureLogs(t)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	row := deferredRow(t, m, "probeplat-platform")
	if !row.Deferred || len(row.ToolsRegistered) != 0 || !warnedWith(logs.String(), "probeplat-platform") {
		t.Fatalf("row %+v logs %s", row, logs.String())
	}
}

// Rewrites tools.py so register_tools raises after registering its one tool
func breakAfterFirstTool(t *testing.T, p *deferredProbe) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dir, "tools.py"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.dir, "tools.py"), string(raw)+"    raise RuntimeError('boom after the first tool')\n")
}

// test_partially_registered_tools_are_still_attributed
func TestDeferredPlatformToolPreregistrationPartiallyRegisteredToolsAreStillAttributed(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	breakAfterFirstTool(t, p)
	logs := captureLogs(t)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	row := deferredRow(t, m, "probeplat-platform")
	predeclared, _ := deferredPredeclared(m, "probeplat-platform")
	if got := resolveProbe(h); !reflect.DeepEqual(got, []string{"probeplat_call"}) {
		t.Fatalf("resolve_toolset = %v", got)
	}
	if !reflect.DeepEqual(row.ToolsRegistered, []string{"probeplat_call"}) || !reflect.DeepEqual(predeclared, []string{"probeplat_call"}) || !row.Deferred ||
		!warnedWith(logs.String()) {
		t.Fatalf("row %+v predeclared %v logs %s", row, predeclared, logs.String())
	}
}

// test_failed_materialization_tears_down_pre_registered_tools
func TestDeferredPlatformToolPreregistrationFailedMaterializationTearsDownPreRegisteredTools(t *testing.T) {
	h, m, platforms := deferredWorld(t)
	p := writeDeferredPlatform(t, "probeplat", true, true)
	breakAfterFirstTool(t, p)
	captureLogs(t)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(p.mf) })
	platforms.get(homeCtx(h), "probeplat", h)
	row := deferredRow(t, m, "probeplat-platform")
	if got := resolveProbe(h); len(got) != 0 {
		t.Fatalf("resolve_toolset after the failed load = %v", got)
	}
	if len(row.ToolsRegistered) != 0 || row.Enabled || row.Error == "" {
		t.Fatalf("row %+v", row)
	}
	if _, kept := deferredPredeclared(m, "probeplat-platform"); kept {
		t.Fatal("the predeclared tools entry outlived the failed load")
	}
}

// test_failed_load_is_rearmed_and_heals_on_next_lookup, the platform adapter registering from a compiled-in plugin as every Icebath adapter does
func TestDeferredPlatformToolPreregistrationFailedLoadIsRearmedAndHealsOnNextLookup(t *testing.T) {
	h, m, platforms := deferredWorld(t)
	var registers atomic.Int64
	ledgerSwapBundled(t, ledgerBundled{m: Manifest{Name: "probeplat-platform", Kind: "platform"}, register: func(ctx *Context) error {
		if registers.Add(1) == 1 {
			return errors.New("load deadline overrun")
		}
		_, err := ctx.RegisterPlatform(PlatformSpec{Name: "probeplat", Label: "Probe", AdapterFactory: func(any) any { return struct{}{} }, Check: func() bool { return true }})
		return err
	}})
	var mf *Manifest
	for _, b := range bundledManifests() {
		mf = b
	}
	ctx := homeCtx(h)
	if err := m.registerDeferredPlatform(ctx, mf); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Unload(mf) })
	if platforms.get(ctx, "probeplat", h) != nil || platforms.get(ctx, "probeplat", h) != nil || registers.Load() != 1 {
		t.Fatalf("a failed load resolved, or a lookup retried it (%d registers)", registers.Load())
	}
	cfg := filepath.Join(h, "config.yaml")
	writeFile(t, cfg, "plugins:\n  isolation: host\n  disabled: [probeplat-platform]\n")
	if GateManifest(h, mf, DisabledPlugins(h), nil, false).Action != "placeholder" || m.RearmFailedPlatform(ctx, "probeplat") {
		t.Fatal("a platform the user disabled was re-armed")
	}
	writeFile(t, cfg, "plugins:\n  isolation: host\n")
	savedVersion := RunningVersion
	RunningVersion, mf.RequiresIcebath = "0.30.0", ">=999.0"
	rearmedForAnotherVersion := m.RearmFailedPlatform(ctx, "probeplat")
	RunningVersion, mf.RequiresIcebath = savedVersion, ""
	if rearmedForAnotherVersion {
		t.Fatal("a plugin built for another Icebath version was re-armed")
	}
	hung := &abandonedLoader{name: loaderGoroutinePrefix + "probeplat-platform", done: make(chan struct{})}
	abandonedMu.Lock()
	saved := abandonedLoaders
	abandonedLoaders = append(append([]*abandonedLoader{}, saved...), hung)
	abandonedMu.Unlock()
	rearmedWhileHung := m.RearmFailedPlatform(ctx, "probeplat")
	close(hung.done)
	abandonedMu.Lock()
	abandonedLoaders = saved
	abandonedMu.Unlock()
	if rearmedWhileHung {
		t.Fatal("re-armed while its hung load still runs")
	}
	if !m.RearmFailedPlatform(ctx, "probeplat") {
		t.Fatalf("the failed load was not re-armed (error %q)", loadedError(m, "probeplat-platform"))
	}
	if platforms.get(ctx, "probeplat", h) == nil || registers.Load() != 2 {
		t.Fatalf("healed lookup registers %d error %q", registers.Load(), loadedError(m, "probeplat-platform"))
	}
	if m.RearmFailedPlatform(ctx, "probeplat") {
		t.Fatal("a loaded platform was re-armed")
	}
}

// test_declared_tools_with_no_tools_module_warns
func TestDeferredPlatformToolPreregistrationDeclaredToolsWithNoToolsModuleWarns(t *testing.T) {
	h, m, _ := deferredWorld(t)
	p := writeDeferredPlatform(t, "promiseplat", false, true)
	logs := captureLogs(t)
	if err := m.registerDeferredPlatform(homeCtx(h), p.mf); err != nil {
		t.Fatal(err)
	}
	row := deferredRow(t, m, "promiseplat-platform")
	if p.count("package_execs") != 0 || len(row.ToolsRegistered) != 0 || !warnedWith(logs.String(), "promiseplat-platform", "provides_tools") {
		t.Fatalf("package %d row %+v logs %s", p.count("package_execs"), row, logs.String())
	}
}
