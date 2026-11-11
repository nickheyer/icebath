package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

const memoryProbeProvider = `
import os
from agent.memory_provider import MemoryProvider

class Probe(MemoryProvider):
    level = 0
    @property
    def name(self): return "memprobe"
    def is_available(self): return True
    def initialize(self, session_id, *, warning_callback=None): self._cb = warning_callback
    def loose(self, **kw): return sorted(kw)
    def get_tool_schemas(self): return []
    def whoami(self):
        self.level += 1
        return os.getpid()
    def ping(self, cb): return cb(2, k="v")
    def later(self): return self._cb("stored")
    def drop(self):
        import gc
        self._cb = None
        gc.collect()

def register(ctx):
    ctx.register_memory_provider(Probe())
`

// Writes a user memory provider package under the live home plugins dir and returns its directory
func memoryProviderDir(t *testing.T, h, name, body string) string {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	memWrite(t, filepath.Join(dir, "__init__.py"), body)
	return dir
}

// Writes body to path creating its parent directories
func memWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body)
}

// Registry plugin manager of the live home whose host the memory loader uses, shut down with the test
func memoryManager(t *testing.T, h string) *Manager {
	t.Helper()
	m := GetPluginManager(homeCtx(h))
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	return m
}

func memoryModule(name string) string { return UserMemoryNamespace + "." + name }

// test_plugin_host.py test_hosted_memory_provider_stays_live_across_a_host_crash
func TestHostedMemoryProviderStaysLiveAcrossAHostCrash(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	dir := memoryProviderDir(t, h, "memprobe", memoryProbeProvider)
	marker := filepath.Join(t.TempDir(), "schema_pid")
	memWrite(t, filepath.Join(dir, "config_schema.py"), "import os, pathlib\npathlib.Path("+strconv.Quote(marker)+").write_text(str(os.getpid()))\nCONFIG_SCHEMA = None\n")
	obj, err := CollectMemoryProviderInHost(ctx, "memprobe", dir, memoryModule("memprobe"), false, nil)
	if err != nil || obj == nil {
		t.Fatalf("collect = %v %v", obj, err)
	}
	host, _ := m.pluginHost()
	first, err := obj.Call(ctx, "whoami", nil, nil)
	if err != nil || first != int64(host.PID()) || first == int64(os.Getpid()) {
		t.Fatalf("whoami = %v %v (host %d)", first, err, host.PID())
	}
	if level, _ := obj.Get(ctx, "level"); level != int64(1) {
		t.Fatalf("level = %v", level)
	}
	if schema, err := MemoryProviderConfigSchema(ctx, filepath.Join(dir, "config_schema.py")); schema != nil || err != nil {
		t.Fatalf("schema = %v %v", schema, err)
	}
	if pid, _ := os.ReadFile(marker); string(pid) != strconv.Itoa(host.PID()) {
		t.Fatalf("schema ran in %q, host %d", pid, host.PID())
	}
	_ = proc.KillPID(int(first.(int64)))
	deadline := time.Now().Add(10 * time.Second)
	for host.Alive() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	again, err := obj.Call(ctx, "whoami", nil, nil)
	if err != nil || again == first || again == int64(os.Getpid()) {
		t.Fatalf("whoami after crash = %v %v", again, err)
	}
}

const memoryCollectorProvider = `
import os
from agent.memory_provider import MemoryProvider

class Collected(MemoryProvider):
    @property
    def name(self): return "collected"
    def is_available(self): return True
    def initialize(self, session_id, **kw): pass
    def get_tool_schemas(self): return []
    def report(self): return list(SEEN)
    def whoami(self): return os.getpid()

SEEN = []

def register(ctx):
    ctx.register_memory_provider(Collected())
    ctx.register_hook("on_session_start", lambda **kw: None)
    ctx.register_skill("maint", SKILL_DIR)
    SEEN.append(repr(ctx.register_cli_command("collected", "help", lambda p: None)))
    SEEN.append(repr(ctx.register_auxiliary_task("bad")))
    for probe in ("get_config", "llm"):
        try:
            getattr(ctx, probe)("x") if probe == "get_config" else ctx.llm.complete
        except Exception as exc:
            SEEN.append(probe + ":" + type(exc).__name__ + ":" + str(exc))
`

// The collector forwards hooks as fallback hooks, gates skills, ignores CLI commands and survives a failing registration
func TestMemoryCollectorRoutesHostedRegistrations(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	skill := filepath.Join(t.TempDir(), "maint")
	memWrite(t, filepath.Join(skill, "SKILL.md"), "---\nname: maint\ndescription: d\n---\nbody\n")
	body := strings.Replace(memoryCollectorProvider, "SEEN = []", "SEEN = []\nSKILL_DIR = "+strconv.Quote(skill), 1)
	for pass, registerSkills := range []bool{false, true} {
		dir := memoryProviderDir(t, h, "collected", body)
		tracked := []string{}
		obj, err := CollectMemoryProviderInHost(ctx, "collected", dir, memoryModule("collected"), registerSkills, func(q string) { tracked = append(tracked, q) })
		if err != nil || obj == nil {
			t.Fatalf("collect = %v %v", obj, err)
		}
		seen, err := obj.Call(ctx, "report", nil, nil)
		want := []string{"None", "None", "get_config:PluginHostError:AttributeError: get_config", "llm:PluginHostError:AttributeError: llm"}
		if err != nil || len(seen.([]any)) != 4*(pass+1) {
			t.Fatalf("report = %v %v", seen, err)
		}
		for i, w := range want {
			if got := seen.([]any)[4*pass+i]; got != w {
				t.Fatalf("report[%d] = %v, want %v", i, got, w)
			}
		}
		if registerSkills != (len(tracked) == 1 && tracked[0] == "collected:maint") {
			t.Fatalf("register_skills %v tracked %v", registerSkills, tracked)
		}
		if _, found := m.FindPluginSkill("collected:maint"); found != registerSkills {
			t.Fatalf("register_skills %v skill registered %v", registerSkills, found)
		}
		if n := len(m.hooks["on_session_start"]); n != 1 {
			t.Fatalf("on_session_start hooks = %d", n)
		}
	}
	host, _ := m.pluginHost()
	_ = proc.KillPID(host.PID())
	deadline := time.Now().Add(10 * time.Second)
	for host.Alive() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	obj, err := CollectMemoryProviderInHost(ctx, "collected", filepath.Join(h, "plugins", "collected"), memoryModule("collected"), false, nil)
	if err != nil || obj == nil {
		t.Fatalf("collect after crash = %v %v", obj, err)
	}
	if n := len(m.hooks["on_session_start"]); n != 1 {
		t.Fatalf("on_session_start hooks after reload = %d", n)
	}
}

// Go callables passed to a hosted provider run here when called, now or later, and the child releases them
func TestHostCallableAndAcceptsKeyword(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	dir := memoryProviderDir(t, h, "memprobe", memoryProbeProvider)
	obj, err := CollectMemoryProviderInHost(ctx, "memprobe", dir, memoryModule("memprobe"), false, nil)
	if err != nil || obj == nil {
		t.Fatalf("collect = %v %v", obj, err)
	}
	for _, c := range []struct {
		method, keyword string
		want            bool
	}{{"initialize", "warning_callback", true}, {"initialize", "session_id", true}, {"initialize", "metadata", false},
		{"loose", "anything", true}, {"whoami", "x", true}, {"ping", "cb", true}, {"ping", "x", false}, {"no_such_method", "x", true}} {
		if got := obj.AcceptsKeyword(c.method, c.keyword); got != c.want {
			t.Fatalf("AcceptsKeyword(%s, %s) = %v", c.method, c.keyword, got)
		}
	}
	var got []any
	echo := HostCallable(func(_ context.Context, args []any, kwargs map[string]any) (any, error) {
		got = append(got, args...)
		return map[string]any{"args": args, "kwargs": kwargs}, nil
	})
	answer, err := obj.Call(ctx, "ping", []any{echo}, nil)
	reply, _ := answer.(map[string]any)
	if err != nil || len(reply["args"].([]any)) != 1 || reply["args"].([]any)[0] != int64(2) || reply["kwargs"].(map[string]any)["k"] != "v" {
		t.Fatalf("ping = %v %v", answer, err)
	}
	if _, err := obj.Call(ctx, "initialize", []any{"s1"}, map[string]any{"warning_callback": echo}); err != nil {
		t.Fatal(err)
	}
	if _, err := obj.Call(ctx, "later", nil, nil); err != nil || len(got) != 2 || got[1] != "stored" {
		t.Fatalf("later = %v %v", got, err)
	}
	host, _ := m.pluginHost()
	if _, err := obj.Call(ctx, "drop", nil, nil); err != nil {
		t.Fatal(err)
	}
	host.stateMu.Lock()
	held := len(host.parents)
	host.stateMu.Unlock()
	if held != 0 {
		t.Fatalf("%d parent callables still held after the child dropped them", held)
	}
}

// Writes a dist-info whose memory provider entry points name modules of the same directory
func memoryEntryPoints(t *testing.T, dir string, points map[string]string) {
	t.Helper()
	lines := "[icebath_agent.memory_providers]\n"
	for name, target := range points {
		lines += name + " = " + target + "\n"
	}
	memWrite(t, filepath.Join(dir, "memeps-1.0.dist-info", "METADATA"), "Metadata-Version: 2.1\nName: memeps\nVersion: 1.0\n")
	memWrite(t, filepath.Join(dir, "memeps-1.0.dist-info", "entry_points.txt"), lines)
}

const entryPointProviders = `
from agent.memory_provider import MemoryProvider

class Base(MemoryProvider):
    label = "base"
    @property
    def name(self): return self.label
    def is_available(self): return True
    def initialize(self, session_id, **kw): pass
    def get_tool_schemas(self): return []

class AsClass(Base):
    label = "subclass"

class Broken(Base):
    def __init__(self): raise RuntimeError("no")

INSTANCE = Base()
INSTANCE.label = "instance"

def factory():
    p = Base()
    p.label = "factory"
    return p

def register_callable(ctx):
    p = Base()
    p.label = "regcallable"
    ctx.register_memory_provider(p)
`

// _load_provider_from_entry_point shapes: instance, subclass, register module, factory, register callable, namespace, none
func TestMemoryProviderEntryPointShapes(t *testing.T) {
	installLiveServices(t)
	site := t.TempDir()
	t.Setenv("PYTHONPATH", site)
	memWrite(t, filepath.Join(site, "memep_shapes.py"), entryPointProviders)
	memWrite(t, filepath.Join(site, "memep_regmod.py"), "from memep_shapes import Base\ndef register(ctx):\n    p = Base()\n    p.label = 'regmodule'\n    ctx.register_memory_provider(p)\n    ctx.register_hook('on_session_end', lambda **kw: None)\n")
	memWrite(t, filepath.Join(site, "memep_namespace.py"), "import memep_shapes as shapes\nclass Broken(shapes.Base):\n    def __init__(self): raise RuntimeError('no')\nclass Later(shapes.Base):\n    label = 'namespace'\n")
	memWrite(t, filepath.Join(site, "memep_empty.py"), "VALUE = 1\n")
	memoryEntryPoints(t, site, map[string]string{"instance": "memep_shapes:INSTANCE", "subclass": "memep_shapes:AsClass",
		"regmodule": "memep_regmod", "factory": "memep_shapes:factory", "regcallable": "memep_shapes:register_callable",
		"namespace": "memep_namespace", "empty": "memep_empty"})
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	for _, name := range []string{"instance", "subclass", "regmodule", "factory", "regcallable", "namespace"} {
		obj, err := CollectMemoryProviderEntryPoint(ctx, name, false, nil)
		if err != nil || obj == nil {
			t.Fatalf("%s = %v %v", name, obj, err)
		}
		if got, err := obj.Get(ctx, "name"); got != name || err != nil {
			t.Fatalf("%s name = %v %v", name, got, err)
		}
	}
	if n := len(m.hooks["on_session_end"]); n != 1 {
		t.Fatalf("register module hooks = %d", n)
	}
	if obj, err := CollectMemoryProviderEntryPoint(ctx, "empty", false, nil); obj != nil || err != nil {
		t.Fatalf("empty = %v %v", obj, err)
	}
	if _, err := CollectMemoryProviderEntryPoint(ctx, "missing", false, nil); err == nil || !strings.Contains(err.Error(), "Entry point 'missing' not found") {
		t.Fatalf("missing = %v", err)
	}
}

// test_session_construction_off_loop.py test_import_memory_provider_module_imports_without_constructing in the host
func TestWarmMemoryProviderImportImportsWithoutConstructing(t *testing.T) {
	installLiveServices(t)
	site, marks := t.TempDir(), t.TempDir()
	t.Setenv("PYTHONPATH", site)
	mark := func(name string) string {
		return "import pathlib\npathlib.Path(" + strconv.Quote(filepath.Join(marks, name)) + ").write_text('1')\n"
	}
	memWrite(t, filepath.Join(site, "_warm_native.py"), mark("native"))
	memWrite(t, filepath.Join(site, "warm_ep.py"), mark("entrypoint"))
	memoryEntryPoints(t, site, map[string]string{"warmep": "warm_ep"})
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	memoryManager(t, h)
	ctx := homeCtx(h)
	dir := memoryProviderDir(t, h, "warmprov", mark("imported")+"from agent.memory_provider import MemoryProvider\ndef register(ctx):\n"+
		"    import pathlib\n    pathlib.Path("+strconv.Quote(filepath.Join(marks, "registered"))+").write_text('1')\n")
	if !WarmMemoryProviderImport(ctx, "warmprov", dir, memoryModule("warmprov"), []string{"_warm_native", "_no_such_native"}) {
		t.Fatal("warm import of warmprov failed")
	}
	for name, want := range map[string]bool{"imported": true, "native": true, "registered": false} {
		if _, err := os.Stat(filepath.Join(marks, name)); (err == nil) != want {
			t.Fatalf("marker %s present %v", name, err == nil)
		}
	}
	if !WarmMemoryProviderImport(ctx, "warmep", "", "", nil) {
		t.Fatal("warm import of the entry point failed")
	}
	if _, err := os.Stat(filepath.Join(marks, "entrypoint")); err != nil {
		t.Fatal(err)
	}
	broken := memoryProviderDir(t, h, "brokenwarm", "raise ImportError('nope')\n")
	if WarmMemoryProviderImport(ctx, "brokenwarm", broken, memoryModule("brokenwarm"), nil) || WarmMemoryProviderImport(ctx, "no-such-provider", "", "", nil) {
		t.Fatal("a failing or unknown provider warmed")
	}
}

// cli.py imports in the profile host without the package and its argparse command runs in a fresh child
func TestMemoryProviderCLIDescribeAndRun(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	memoryManager(t, h)
	ctx := homeCtx(h)
	dir := memoryProviderDir(t, h, "clip", "raise RuntimeError('package must not run')\n")
	memWrite(t, filepath.Join(dir, "config.py"), "STATUS = 'ok'\n")
	memWrite(t, filepath.Join(dir, "cli.py"), "from . import config\ndef register_cli(p):\n    p.add_argument('--status', action='store_true')\n"+
		"    p.add_argument('--boom', action='store_true')\ndef clip_command(args):\n    if args.boom:\n        raise ValueError('kaboom')\n"+
		"    print(config.STATUS, args.status)\n    return 4\n")
	registerCLI, handler, err := DescribeMemoryProviderCLI(ctx, "clip", dir, memoryModule("clip"))
	if err != nil || !registerCLI || !handler {
		t.Fatalf("describe = %v %v %v", registerCLI, handler, err)
	}
	run := func(handler bool, argv ...string) (int, bool, string, string) {
		var stdout, stderr bytes.Buffer
		rc, dispatched, err := RunMemoryProviderCLI(ctx, "clip", dir, memoryModule("clip"), "Clip tools", handler, argv, nil, &stdout, &stderr)
		if err != nil {
			t.Fatalf("run %v: %v", argv, err)
		}
		return rc, dispatched, stdout.String(), stderr.String()
	}
	if rc, dispatched, out, _ := run(true, "--status"); rc != 4 || !dispatched || out != "ok True\n" {
		t.Fatalf("--status = %d %v %q", rc, dispatched, out)
	}
	if rc, dispatched, out, _ := run(true, "--help"); rc != 0 || !dispatched || !strings.Contains(out, "usage: icebath clip") || !strings.Contains(out, "Clip tools") {
		t.Fatalf("--help = %d %v %q", rc, dispatched, out)
	}
	if rc, dispatched, _, errOut := run(true, "--bogus"); rc != 2 || !dispatched || !strings.Contains(errOut, "unrecognized arguments: --bogus") {
		t.Fatalf("--bogus = %d %v %q", rc, dispatched, errOut)
	}
	if rc, dispatched, _, errOut := run(true, "--boom"); rc != 1 || !dispatched || !strings.Contains(errOut, "ValueError: kaboom") {
		t.Fatalf("--boom = %d %v %q", rc, dispatched, errOut)
	}
	if rc, dispatched, _, _ := run(false); rc != 0 || dispatched {
		t.Fatalf("no handler = %d %v", rc, dispatched)
	}
	memWrite(t, filepath.Join(h, "plugins", "nocli", "cli.py"), "raise ImportError('broken cli')\n")
	if _, _, err := DescribeMemoryProviderCLI(ctx, "nocli", filepath.Join(h, "plugins", "nocli"), memoryModule("nocli")); err == nil || !strings.Contains(err.Error(), "broken cli") {
		t.Fatalf("broken cli = %v", err)
	}
}
