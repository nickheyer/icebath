package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const demoEngineSrc = `
from agent.context_engine import ContextEngine
class Demo(ContextEngine):
    @property
    def name(self):
        return "ctx_demo"
    def update_from_response(self, usage):
        pass
    def should_compress(self, prompt_tokens=None):
        return False
    def compress(self, messages, current_tokens=None):
        return messages
def register(ctx):
    ctx.register_context_engine(Demo())
`

// Host isolated home whose plugin host child runs the system python, its manager host shut down at cleanup
func engineHome(t *testing.T) (context.Context, string) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "plugins:\n  isolation: host\n  enabled: []\n"})
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	python := liveInterpreters(t)["system"]
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if m := GetPluginManager(ctx); m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
		hostInterpreter = previous
	})
	return ctx, h
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, body)
	}
}

// test_context_engine_user_dir.py loader half: a user engine loads by name, an unrelated user plugin is never an engine
func TestUserInstalledEngineIsSelectedByName(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	ctx, h := engineHome(t)
	writeTree(t, filepath.Join(h, "plugins"), map[string]string{"ctx_demo/__init__.py": demoEngineSrc, "notes/__init__.py": "def register(ctx): pass\n"})
	if LoadContextEngine(ctx, "notes") != nil {
		t.Fatal("an unrelated user plugin loaded as an engine")
	}
	if _, ok := FindEngineDir(ctx, "missing"); ok {
		t.Fatal("missing engine resolved")
	}
	infos, err := DiscoverContextEngines(ctx)
	if err != nil || len(infos) != 1 || infos[0].Name != "ctx_demo" || !infos[0].Available {
		t.Fatalf("discovered %v %v", infos, err)
	}
	engine := LoadContextEngine(ctx, "ctx_demo")
	if engine == nil {
		t.Fatal("ctx_demo did not load")
	}
	if name, err := engine.Get(ctx, "name"); err != nil || name != "ctx_demo" {
		t.Fatalf("engine name %v %v", name, err)
	}
}

// Bundled engines load through load_plugin_module and instance_from_module with the engine collector serving the rest
func TestBundledEngineLoadsInProcessWithItsCollector(t *testing.T) {
	bundled := t.TempDir()
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", bundled)
	ctx, _ := engineHome(t)
	writeTree(t, bundled, map[string]string{
		"context_engine/__init__.py":     "MARK = 'parent'\n",
		"context_engine/lcm/helper.py":   "NAME = 'lcm-helper'\n",
		"context_engine/lcm/__init__.py": strings.Replace(demoEngineSrc, `return "ctx_demo"`, "return NAME", 1) + "from .helper import NAME\n" + bundledEngineRegister,
		"context_engine/bare/__init__.py": strings.Replace(strings.Replace(demoEngineSrc, `return "ctx_demo"`, `return "bare"`, 1),
			"    ctx.register_context_engine(Demo())", "    ctx.register_context_engine(None)\n    raise RuntimeError('late')", 1),
	})
	engine := LoadContextEngine(ctx, "lcm")
	if engine == nil {
		t.Fatal("bundled lcm did not load")
	}
	if name, err := engine.Get(ctx, "name"); err != nil || name != "lcm-helper" {
		t.Fatalf("engine name %v %v", name, err)
	}
	if flags, err := engine.Get(ctx, "flags"); err != nil || flags != "parent:False:True:noop" {
		t.Fatalf("collector surface %v %v", flags, err)
	}
	cmd := GetPluginManager(ctx).PluginCommands()["lcm-status"]
	if cmd == nil || cmd.Plugin != "context-engine:lcm" || cmd.ArgsHint != "<x>" || cmd.Description != "Context engine command" {
		t.Fatalf("engine command %+v", cmd)
	}
	if out, err := cmd.Handler(ctx, "abc"); err != nil || out != "s:abc" {
		t.Fatalf("engine command run %v %v", out, err)
	}
	if bare := LoadContextEngine(ctx, "bare"); bare == nil {
		t.Fatal("a raising register must fall back to the ContextEngine subclass")
	} else if name, _ := bare.Get(ctx, "name"); name != "bare" {
		t.Fatalf("fallback engine %v", name)
	}
}

const bundledEngineRegister = `
import sys
def register(ctx):
    engine = Demo()
    ctx.register_context_engine(engine)
    ctx.register_command("/LCM Status", lambda raw: "s:" + raw, args_hint=" <x> ")
    ctx.register_tool("x", "toolset", {}, None)
    engine.flags = ":".join([sys.modules["plugins.context_engine"].MARK, str(hasattr(ctx, "register_skill")),
                             str(hasattr(sys.modules["plugins.context_engine"], "lcm")), "noop"])
`

// test_context_engine_host_contract.py test_engine_collector_forwards_register_command_to_plugin_manager plus its refusals
func TestEngineCollectorForwardsRegisterCommandToPluginManager(t *testing.T) {
	m := NewManager(t.TempDir())
	handler := PluginCommandHandler(func(_ context.Context, raw string) (any, error) { return "echo: " + raw, nil })
	collector := &engineCollector{name: "my-lcm"}
	collector.registerCommand(m, "my-lcm-test-cmd", handler, "test command from a context engine", "<msg>")
	entry := m.PluginCommands()["my-lcm-test-cmd"]
	if entry == nil || entry.ArgsHint != "<msg>" || entry.Plugin != "context-engine:my-lcm" || entry.Description != "test command from a context engine" {
		t.Fatalf("entry %+v", entry)
	}
	if out, _ := entry.Handler(context.Background(), "hi"); out != "echo: hi" {
		t.Fatalf("handler %v", out)
	}
	previous := installedServices()
	t.Cleanup(func() { InstallServices(previous) })
	InstallServices(&Services{ResolveBuiltinCommand: func(name string) bool { return name == "help" }})
	collector.registerCommand(m, "  ", handler, "", "")
	collector.registerCommand(m, "/Help", handler, "", "")
	collector.registerCommand(m, "my-lcm-test-cmd", func(context.Context, string) (any, error) { return "second", nil }, "", "")
	commands := m.PluginCommands()
	if len(commands) != 1 {
		t.Fatalf("refused registrations landed %v", commands)
	}
	if out, _ := commands["my-lcm-test-cmd"].Handler(context.Background(), ""); out != "echo: " {
		t.Fatal("a plugin command was replaced")
	}
	if c := newEngineCollectorContext(m, ""); c.engineCollector.name != "context_engine" {
		t.Fatalf("default engine name %q", c.engineCollector.name)
	}
}

// LoadNamed warns and answers nil for a failing load or a missing instance
func TestLoadNamedWarnsOnFailureOrNoInstance(t *testing.T) {
	if LoadNamed("x", "/d", func(string) (any, error) { return nil, os.ErrNotExist }, "Context engine", "engine") != nil {
		t.Fatal("error load")
	}
	if LoadNamed("x", "/d", func(string) (any, error) { return nil, nil }, "Context engine", "engine") != nil {
		t.Fatal("empty load")
	}
	if LoadNamed("x", "/d", func(string) (any, error) { return "", nil }, "Context engine", "engine") != nil {
		t.Fatal("falsy load")
	}
	if got := LoadNamed("x", "/d", func(dir string) (any, error) { return dir, nil }, "Context engine", "engine"); got != "/d" {
		t.Fatalf("instance %v", got)
	}
}

// Runs a Python driver with the embedded plugin SDK and child package on its import path
func runSDKDriver(t *testing.T, script string, args ...string) string {
	t.Helper()
	sdk, err := pluginSDKPythonPath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(t.TempDir(), "driver.py")
	writeFile(t, driver, script)
	boot := "import sys; sys.path[:0] = sys.argv[1:3]; sys.argv = sys.argv[3:]; exec(compile(open(sys.argv[0]).read(), sys.argv[0], 'exec'))"
	cmd := exec.Command(liveInterpreters(t)["system"], append([]string{"-I", "-c", boot, sdk, filepath.Dir(sdk), driver}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("driver: %v\n%s%s", err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// test_plugin_loader.py all three tests over the plugin SDK loader the host child runs
func TestPluginLoaderModuleLoads(t *testing.T) {
	if out := runSDKDriver(t, pluginLoaderDriver, t.TempDir()); out != "ok" {
		t.Fatalf("driver output %q", out)
	}
}

// The CronScheduler SDK base: abstract surface, signature detection and split fire detection
func TestCronSchedulerSDKBase(t *testing.T) {
	if out := runSDKDriver(t, cronSchedulerDriver); out != "ok" {
		t.Fatalf("driver output %q", out)
	}
}

const pluginLoaderDriver = `
import importlib, logging, sys, threading
from pathlib import Path
from types import SimpleNamespace
import plugins.plugin_loader as loader
from plugins.plugin_loader import load_plugin_module

tmp = Path(sys.argv[1])
log = logging.getLogger("t")

def clean(prefix):
    for name in tuple(sys.modules):
        if name == prefix or name.startswith(prefix + "."):
            sys.modules.pop(name, None)

plugin_dir = tmp / "failed"
plugin_dir.mkdir()
(plugin_dir / "broken.py").write_text("from .missing_dependency import value\n")
(plugin_dir / "__init__.py").write_text("try:\n    from .broken import value\nexcept ModuleNotFoundError:\n    fallback_used = True\n")
name = "test_plugin_loader_package.failed_sibling"
module = load_plugin_module(name, plugin_dir, parents=(), logger=log)
assert module is not None and module.fallback_used is True
assert name + ".broken" not in sys.modules
clean(name)

plugin_dir = tmp / "success"
plugin_dir.mkdir()
(plugin_dir / "helper.py").write_text("value = 42\n")
(plugin_dir / "__init__.py").write_text("from .helper import value\n")
name = "test_plugin_loader_package.successful_sibling"
module = load_plugin_module(name, plugin_dir, parents=(), logger=log)
assert module is not None and module.value == 42 and module.helper.value == 42
clean(name)

loader._CONCURRENT_LOAD_WAIT_SECS = 0.3
plugin_dir = tmp / "plugin"
plugin_dir.mkdir()
gate_name = "_test_plugin_loader_race_gate"
gate = SimpleNamespace(entered=threading.Event(), release=threading.Event())
sys.modules[gate_name] = gate
(plugin_dir / "slow.py").write_text(f"import sys\ngate = sys.modules[{gate_name!r}]\ngate.entered.set()\ngate.release.wait(10)\n")
(plugin_dir / "__init__.py").write_text("def register(ctx):\n    pass\n")
name = "test_plugin_loader_package.concurrent_load"
results = {}

def load(key):
    results[key] = load_plugin_module(name, plugin_dir, parents=(), logger=log)

def bounded(key):
    with loader.bounded_load_wait():
        load(key)

first = threading.Thread(target=load, args=("first",))
build = threading.Thread(target=load, args=("build",))
turn = threading.Thread(target=bounded, args=("turn",))
try:
    first.start()
    assert gate.entered.wait(5)
    build.start()
    build.join(1)
    assert build.is_alive(), f"agent build gave up on a slow import: {results.get('build')!r}"
    turn.start()
    turn.join(5)
    assert not turn.is_alive(), "per-turn caller blocked on the hung first load"
    assert results["turn"] is None
    bounded("stalled")
    assert results["stalled"] is None
    gate.release.set()
    first.join(10)
    build.join(10)
    assert hasattr(results["first"], "register")
    assert results["build"] is results["first"]
    bounded("after")
    assert results["after"] is results["first"]

    loader._CONCURRENT_LOAD_WAIT_SECS = 60
    cyc_dir, helper = tmp / "cyc", "_test_plugin_loader_cycle_helper"
    cyc_dir.mkdir()
    cyc_name = "test_plugin_loader_package.cycle"
    gate.helper, gate.loader = threading.Event(), threading.Event()
    sys.path.insert(0, str(tmp))
    (tmp / f"{helper}.py").write_text(
        f"import sys, logging, pathlib\nfrom plugins.plugin_loader import load_plugin_module\n"
        f"gate = sys.modules[{gate_name!r}]\ngate.helper.set()\ngate.loader.wait(10)\n"
        f"mod = load_plugin_module({cyc_name!r}, pathlib.Path({str(cyc_dir)!r}), parents=(),"
        " logger=logging.getLogger('x'))\n")
    (cyc_dir / "__init__.py").write_text(
        f"import sys\nsys.modules[{gate_name!r}].loader.set()\nimport {helper}\ndef register(ctx):\n    pass\n")
    a = threading.Thread(target=importlib.import_module, args=(helper,), daemon=True)
    b = threading.Thread(target=load_plugin_module, args=(cyc_name, cyc_dir), kwargs={"parents": (), "logger": log}, daemon=True)
    a.start()
    assert gate.helper.wait(5)
    b.start()
    a.join(5)
    b.join(5)
    assert not a.is_alive() and not b.is_alive(), "loader lock and import lock deadlocked"
    assert hasattr(sys.modules[cyc_name], "register")
finally:
    gate.release.set()
    getattr(gate, "loader", threading.Event()).set()

class Collector(loader.NoopPluginContext):
    def __init__(self):
        self.engine = None
    def register_context_engine(self, engine):
        self.engine = engine
class Base:
    pass
mod = SimpleNamespace(register=lambda ctx: (ctx.register_tool(1, 2, k=3), ctx.register_context_engine("E")))
assert loader.instance_from_module(mod, collector=Collector(), collected_attr="engine", base_cls=Base, name="n", logger=log) == "E"
class Sub(Base):
    pass
def raising(ctx):
    ctx.register_context_engine("E")
    raise RuntimeError("late")
mod = SimpleNamespace(register=raising, Sub=Sub, Base=Base)
assert isinstance(loader.instance_from_module(mod, collector=Collector(), collected_attr="engine", base_cls=Base, name="n", logger=log), Sub)
assert loader.load_named("n", tmp, lambda d: None, kind="Context engine", noun="engine", logger=log) is None
assert loader.load_named("n", tmp, lambda d: 1 / 0, kind="Context engine", noun="engine", logger=log) is None
assert loader.load_named("n", tmp, lambda d: "x", kind="Context engine", noun="engine", logger=log) == "x"
assert loader.probe_availability(lambda: SimpleNamespace(is_available=lambda: False)) is False
assert loader.probe_availability(lambda: object()) is True
assert loader.probe_availability(lambda: 1 / 0) is False
loader.register_synthetic_package("_synthetic_ns", [])
assert sys.modules["_synthetic_ns"].__path__ == []
(tmp / "desc").mkdir()
(tmp / "desc" / "plugin.yaml").write_text("description: hi\n")
assert loader.read_plugin_description(tmp / "desc") in ("hi", "")
assert loader.iter_plugin_dirs(tmp / "nope") == []
print("ok")
`

const cronSchedulerDriver = `
import threading
from types import SimpleNamespace
from cron.scheduler_provider import (CronScheduler, provider_fire_due_accepts, provider_supports_force_fire,
                                     provider_supports_split_fire)
from icebath_cli.plugin_host_wire import PluginHostUnavailable

try:
    CronScheduler()
    raise SystemExit("abstract base instantiated")
except TypeError:
    pass

class Modern(CronScheduler):
    name = "modern"
    def start(self, stop_event, *, adapters=None, loop=None, interval=60):
        return None

class Legacy(Modern):
    def fire_due(self, job_id):
        return True

class Kw(Modern):
    def fire_due(self, job_id, **kwargs):
        return True

class Split(Legacy):
    def claim_fire(self, job_id, *, force=False, manual=False):
        return None

assert Modern().is_available() is True and Modern().supports_force_fire is True
assert provider_supports_force_fire(Legacy()) is False and provider_supports_force_fire(Kw()) is True
assert provider_fire_due_accepts(Modern(), "manual") is True and provider_fire_due_accepts(SimpleNamespace(fire_due=1), "force") is False
assert provider_supports_split_fire(Modern()) is True and provider_supports_split_fire(Legacy()) is False
assert provider_supports_split_fire(Split()) is True
assert Modern().stop() is None and Modern().on_jobs_changed() is None and Modern().register_job({}) is None and Modern().reconcile() is None
try:
    provider_fire_due_accepts(object(), "force")
    raise SystemExit("a provider without fire_due was inspected")
except AttributeError:
    pass
try:
    Modern().claim_fire("j")
    raise SystemExit("claim ran without a channel")
except PluginHostUnavailable as exc:
    assert "plugin SDK call 'cron.claim_fire' needs the Icebath plugin host channel" in str(exc)
print("ok")
`

// A hosted cron provider's default fire_due claims and runs through the Icebath cron ops over the SDK channel
func TestHostedCronSchedulerDefaultFireReachesIcebath(t *testing.T) {
	ctx, h := engineHome(t)
	var ops []string
	var claimKwargs map[string]any
	previous := installedServices()
	t.Cleanup(func() { InstallServices(previous) })
	InstallServices(&Services{PluginSDK: func(_ context.Context, _ string, op string, args []any, kwargs map[string]any) (any, error) {
		ops = append(ops, op+":"+strings.TrimSpace(strings.Join(func() []string {
			out := []string{}
			for _, a := range args {
				if m, ok := a.(map[string]any); ok {
					out = append(out, m["id"].(string))
				} else {
					out = append(out, a.(string))
				}
			}
			return out
		}(), ",")))
		if op == "cron.claim_fire" {
			claimKwargs = kwargs
			return map[string]any{"id": args[0]}, nil
		}
		return nil, nil
	}})
	dir := filepath.Join(h, "plugins", "chrono")
	writeTree(t, dir, map[string]string{"__init__.py": "from cron.scheduler_provider import CronScheduler\n" +
		"class Chrono(CronScheduler):\n    name = 'chrono'\n    def start(self, stop_event, *, adapters=None, loop=None, interval=60):\n        return None\n" +
		"def register(ctx):\n    ctx.register_cron_scheduler(Chrono())\n"})
	host, err := GetPluginManager(ctx).PluginHost()
	if err != nil {
		t.Fatal(err)
	}
	obj, err := host.LoadInstance(ctx, dir, "_icebath_user_cron.chrono", "cron.scheduler_provider:CronScheduler", "register_cron_scheduler", nil, nil)
	if err != nil || obj == nil {
		t.Fatalf("load %v %v", obj, err)
	}
	fired, err := obj.Call(ctx, "fire_due", []any{"job1"}, map[string]any{"manual": true})
	if err != nil || fired != true {
		t.Fatalf("fire_due %v %v", fired, err)
	}
	if strings.Join(ops, " ") != "cron.claim_fire:job1 cron.fire_claimed:job1" || claimKwargs["source"] != "chrono" || claimKwargs["manual"] != true || claimKwargs["force"] != false {
		t.Fatalf("ops %v kwargs %v", ops, claimKwargs)
	}
}
