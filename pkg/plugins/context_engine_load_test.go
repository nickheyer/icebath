package plugins

import (
	"context"
	"os"
	"path/filepath"
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
