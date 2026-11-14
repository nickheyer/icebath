package plugins

import (
	"os"
	"path/filepath"
	"testing"
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

func memoryModule(name string) string { return UserMemoryNamespace + "." + name }

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
