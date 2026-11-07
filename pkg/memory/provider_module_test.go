package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Connection store shaped like the holographic plugin store.py, release_all_under closing every handle under a directory
const holographicStoreSource = `import os, sqlite3, threading
from pathlib import Path


class MemoryStore:
    _shared = {}
    _shared_guard = threading.Lock()

    def __init__(self, db_path):
        key = str(Path(db_path).expanduser().resolve())
        with MemoryStore._shared_guard:
            if key not in MemoryStore._shared:
                MemoryStore._shared[key] = {"conn": sqlite3.connect(key, check_same_thread=False), "lock": threading.Lock()}

    @classmethod
    def release_all_under(cls, directory: "str | Path") -> int:
        root = os.path.normcase(str(Path(directory).expanduser().resolve())) + os.sep
        with cls._shared_guard:
            doomed = [cls._shared.pop(key) for key in list(cls._shared) if os.path.normcase(key).startswith(root)]
            for entry in doomed:
                with entry["lock"]:
                    entry["conn"].close()
        return len(doomed)
`

// The profile delete release the manage binding makes, MemoryStore.release_all_under summed over every running host
func releaseHolographicUnder(t *testing.T, dir string) int {
	t.Helper()
	total := 0
	for _, host := range plugins.RunningPluginHosts() {
		mod, err := ImportProviderModuleIn(t.Context(), host, "holographic", "store")
		if err != nil {
			t.Fatal(err)
		}
		n, err := mod.Call(t.Context(), "MemoryStore.release_all_under", []any{dir}, nil)
		if err != nil {
			t.Fatal(err)
		}
		total += int(n.(int64))
	}
	return total
}

func TestImportProviderModuleReachesTheInstalledHolographicStore(t *testing.T) {
	isolateRegistry(t)
	ctx, h := discHome(t)
	t.Cleanup(func() {
		if host, err := plugins.GetPluginManager(ctx).PluginHost(); err == nil {
			host.Shutdown()
		}
	})
	var ie *ProviderImportError
	if _, err := ImportProviderModule(ctx, "holographic", "store"); !errors.As(err, &ie) || ie.Msg != "memory provider 'holographic' is not installed" {
		t.Fatalf("absent %v", err)
	}
	if _, err := ImportProviderModule(ctx, "byterover", "store"); !errors.As(err, &ie) || ie.Msg != "No module named 'plugins.memory.byterover.store'" {
		t.Fatalf("bundled %v", err)
	}
	dir := discProvider(t, filepath.Join(h, "plugins"), "holographic")
	discWrite(t, filepath.Join(dir, "store.py"), holographicStoreSource)
	profile := filepath.Join(h, "profiles", "work")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	mod, err := ImportProviderModule(ctx, "holographic", "store")
	if err != nil || mod.TypeName != "module" {
		t.Fatalf("module %v %v", mod, err)
	}
	if _, err := mod.Call(ctx, "MemoryStore", []any{filepath.Join(profile, "memory_store.db")}, nil); err != nil {
		t.Fatal(err)
	}
	if n := releaseHolographicUnder(t, profile); n != 1 {
		t.Fatalf("released %d", n)
	}
	if n := releaseHolographicUnder(t, profile); n != 0 {
		t.Fatalf("released again %d", n)
	}
	broken := discProvider(t, filepath.Join(h, "plugins"), "brokenstore")
	discWrite(t, filepath.Join(broken, "__init__.py"), strings.Replace(discoveryProviderSource, "%s", "brokenstore", 1)+"\nraise RuntimeError('import boom')\n")
	if _, err := ImportProviderModule(ctx, "brokenstore", "store"); !errors.As(err, &ie) || ie.Msg != "memory provider 'brokenstore' failed to import" {
		t.Fatalf("broken %v", err)
	}
}
