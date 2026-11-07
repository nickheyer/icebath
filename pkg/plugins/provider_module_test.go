package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCallProviderModuleRunsAUserProviderSubmoduleInTheHost(t *testing.T) {
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if host := GetPluginManager(ctx).pluginHostInstance; host != nil {
			host.Shutdown()
		}
	})
	good := filepath.Join(h, "plugins", "mem")
	broken := filepath.Join(h, "plugins", "brokenmem")
	for _, dir := range []string{good, broken} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(good, "__init__.py"), "VALUE = '-x'\n")
	writeFile(t, filepath.Join(good, "cli.py"), "from . import VALUE\n\n"+
		"class ConfigWriteRefused(Exception):\n    pass\n\n"+
		"def clone(name, suffix=''):\n    return {'name': name + VALUE + suffix, 'pkg': __package__}\n\n"+
		"def refuse(name):\n    raise ConfigWriteRefused('no write for ' + name)\n")
	writeFile(t, filepath.Join(broken, "__init__.py"), "raise RuntimeError('import boom')\n")
	module := UserMemoryNamespace + ".mem__source_0123456789abcdef"
	out, loaded, err := CallProviderModule(ctx, good, module, "cli", "clone", []any{"a"}, map[string]any{"suffix": "!"})
	if err != nil || !loaded {
		t.Fatalf("clone %v %v", loaded, err)
	}
	reply, _ := out.(map[string]any)
	if reply["name"] != "a-x!" || reply["pkg"] != module {
		t.Fatalf("reply %v", out)
	}
	_, loaded, err = CallProviderModule(ctx, good, module, "cli", "refuse", []any{"a"}, nil)
	var he *PluginHostError
	if !loaded || !errors.As(err, &he) || he.TypeName != "ConfigWriteRefused" || he.RemoteMessage != "no write for a" {
		t.Fatalf("refuse %v %v", loaded, err)
	}
	_, loaded, err = CallProviderModule(ctx, broken, UserMemoryNamespace+".brokenmem__source_0", "cli", "clone", nil, nil)
	if loaded || err != nil {
		t.Fatalf("broken package %v %v", loaded, err)
	}
}

// Shared connection store shaped like the holographic plugin MemoryStore, its release classmethod closing every handle under a directory
const holoStoreSource = `import os, sqlite3, threading
from pathlib import Path

VERSION = "0.26.2"


class MemoryStore:
    _shared = {}
    _shared_guard = threading.Lock()

    def __init__(self, db_path):
        key = str(Path(db_path).expanduser().resolve())
        with MemoryStore._shared_guard:
            entry = MemoryStore._shared.get(key)
            if entry is None:
                entry = MemoryStore._shared[key] = {"conn": sqlite3.connect(key, check_same_thread=False), "lock": threading.Lock()}
        self.conn = entry["conn"]

    def tables(self):
        return self.conn.execute("select count(*) from sqlite_master").fetchone()[0]

    @classmethod
    def release_all_under(cls, directory: "str | Path") -> int:
        root = os.path.normcase(str(Path(directory).expanduser().resolve())) + os.sep
        with cls._shared_guard:
            doomed = [cls._shared.pop(key) for key in list(cls._shared) if os.path.normcase(key).startswith(root)]
            for entry in doomed:
                with entry["lock"]:
                    entry["conn"].close()
        return len(doomed)


def open_store(db_path):
    return MemoryStore(db_path)
`

func TestImportProviderModuleCallsAClassMethodInTheHost(t *testing.T) {
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	ctx := homeCtx(h)
	other := filepath.Join(filepath.Dir(h), "other")
	otherCtx := homeCtx(other)
	t.Cleanup(func() {
		for _, c := range []context.Context{ctx, otherCtx} {
			if host := GetPluginManager(c).pluginHostInstance; host != nil {
				host.Shutdown()
			}
		}
	})
	dir := filepath.Join(h, "plugins", "holo")
	broken := filepath.Join(h, "plugins", "brokenholo")
	writeTree(t, dir, map[string]string{"__init__.py": "", "store.py": holoStoreSource})
	writeTree(t, broken, map[string]string{"__init__.py": "raise RuntimeError('import boom')\n"})
	profA, profB := filepath.Join(h, "profiles", "a"), filepath.Join(h, "profiles", "b")
	for _, p := range []string{profA, profB, other} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	module := UserMemoryNamespace + ".holo__source_0123456789abcdef"
	mod, err := ImportProviderModule(ctx, "holo", dir, module, "store")
	if err != nil || mod.TypeName != "module" || !mod.HasMethod("open_store") || mod.HasMethod("VERSION") {
		t.Fatalf("module %v %v", mod, err)
	}
	if v, err := mod.Get(ctx, "VERSION"); err != nil || v != "0.26.2" {
		t.Fatalf("VERSION %v %v", v, err)
	}
	cls, err := mod.Get(ctx, "MemoryStore")
	store, ok := cls.(*HostedObject)
	if err != nil || !ok || !store.HasMethod("release_all_under") || !store.HasMethod("tables") {
		t.Fatalf("class %v %v", cls, err)
	}
	inA, err := mod.Call(ctx, "MemoryStore", []any{filepath.Join(profA, "memory_store.db")}, nil)
	held, ok := inA.(*HostedObject)
	if err != nil || !ok || held.TypeName != "MemoryStore" {
		t.Fatalf("instance %v %v", inA, err)
	}
	if n, err := held.Call(ctx, "tables", nil, nil); err != nil || n != int64(0) {
		t.Fatalf("tables %v %v", n, err)
	}
	if _, err := mod.Call(ctx, "open_store", []any{filepath.Join(profB, "memory_store.db")}, nil); err != nil {
		t.Fatal(err)
	}
	otherMod, err := ImportProviderModule(otherCtx, "holo", dir, module, "store")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherMod.Call(otherCtx, "open_store", []any{filepath.Join(profA, "other.db")}, nil); err != nil {
		t.Fatal(err)
	}
	hosts := RunningPluginHosts()
	mine := map[*PluginHost]bool{GetPluginManager(ctx).pluginHostInstance: true, GetPluginManager(otherCtx).pluginHostInstance: true}
	for _, host := range hosts {
		delete(mine, host)
	}
	if len(mine) != 0 {
		t.Fatalf("running hosts %v miss %v", hosts, mine)
	}
	released := int64(0)
	for _, host := range hosts {
		m, err := host.ImportProviderModule(ctx, "holo", dir, module, "store")
		if err != nil {
			t.Fatal(err)
		}
		n, err := m.Call(ctx, "MemoryStore.release_all_under", []any{profA}, nil)
		if err != nil {
			t.Fatal(err)
		}
		released += n.(int64)
	}
	if released != 2 {
		t.Fatalf("released %d", released)
	}
	if n, err := mod.Call(ctx, "MemoryStore.release_all_under", []any{profA}, nil); err != nil || n != int64(0) {
		t.Fatalf("second release %v %v", n, err)
	}
	var he *PluginHostError
	if _, err := held.Call(ctx, "tables", nil, nil); !errors.As(err, &he) || he.TypeName != "ProgrammingError" {
		t.Fatalf("closed connection %v", err)
	}
	if _, err := mod.Call(ctx, "Nope.release_all_under", []any{profA}, nil); !errors.As(err, &he) || he.TypeName != "AttributeError" {
		t.Fatalf("missing class %v", err)
	}
	if _, err := mod.Call(ctx, "MemoryStore._shared.clear", nil, nil); !errors.As(err, &he) || he.TypeName != "AttributeError" {
		t.Fatalf("private path %v", err)
	}
	if v, err := mod.Call(ctx, "VERSION.upper", nil, nil); err != nil || v != "0.26.2" {
		t.Fatalf("str attribute call %v %v", v, err)
	}
	var builtin *HostBuiltinError
	if _, err := ImportProviderModule(ctx, "brokenholo", broken, UserMemoryNamespace+".brokenholo__source_0", "store"); !errors.As(err, &builtin) ||
		builtin.Type != "ImportError" || builtin.Message != "memory provider 'brokenholo' failed to import" {
		t.Fatalf("broken %v", err)
	}
	if _, err := ImportProviderModule(ctx, "holo", dir, module, "absent"); !errors.As(err, &he) || he.TypeName != "ModuleNotFoundError" {
		t.Fatalf("absent submodule %v", err)
	}
	GetPluginManager(ctx).pluginHostInstance.Shutdown()
	if n, err := mod.Call(ctx, "MemoryStore.release_all_under", []any{profB}, nil); err != nil || n != int64(0) {
		t.Fatalf("after restart %v %v", n, err)
	}
}
