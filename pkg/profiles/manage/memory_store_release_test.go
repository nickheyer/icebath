package manage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const holographicProviderInit = `from agent.memory_provider import MemoryProvider

class Provider(MemoryProvider):
    @property
    def name(self):
        return "holographic"
    def is_available(self):
        return True
    def initialize(self, *a, **kw):
        pass
    def get_tool_schemas(self):
        return []

def register(ctx):
    ctx.register_memory_provider(Provider())
`

// Connection store shaped like the holographic plugin store.py, release_all_under closing every handle under a directory
const holographicStoreModule = `import os, sqlite3, threading
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

// Profile secret source with no managed or source supplied names
type releaseSecretSource struct{}

func (releaseSecretSource) ManagedDotenvKeys() []string   { return nil }
func (releaseSecretSource) SourceSuppliedNames() []string { return nil }
func (releaseSecretSource) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (releaseSecretSource) CurrentSecretScope(ctx context.Context) map[string]string {
	return provider.CurrentSecretScope(ctx)
}

// Running plugin host of the default home with the holographic store installed and one store open per database
func holographicHost(t *testing.T, dbs ...string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	savedKeys, savedAdapters, savedSecrets := tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets
	t.Cleanup(func() {
		tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv, tools.ProfileSecrets = savedKeys, savedAdapters, savedSecrets
	})
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return nil }
	tools.ProfileSecrets = releaseSecretSource{}
	t.Setenv("PYTHONPATH", "")
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
	ctx := context.Background()
	root := filepath.Join(os.Getenv(home.EnvHome), "plugins", "holographic")
	writeFile(t, filepath.Join(root, "__init__.py"), holographicProviderInit)
	writeFile(t, filepath.Join(root, "store.py"), holographicStoreModule)
	t.Cleanup(func() {
		if host, err := plugins.GetPluginManager(ctx).PluginHost(); err == nil {
			host.Shutdown()
		}
	})
	mod, err := memory.ImportProviderModule(ctx, "holographic", "store")
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range dbs {
		if _, err := mod.Call(ctx, "MemoryStore", []any{db}, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryStoreReleaseClosesOnlyTheStoresUnderTheProfile(t *testing.T) {
	tmp := manageEnv(t)
	worker := namedProfile(t, tmp, "worker")
	other := namedProfile(t, tmp, "other")
	holographicHost(t, filepath.Join(worker, "memory_store.db"), filepath.Join(worker, "nested", "..", "second.db"), filepath.Join(other, "memory_store.db"))
	if n, err := memoryStoreReleaseAllUnder(worker); err != nil || n != 2 {
		t.Fatalf("released %d %v", n, err)
	}
	if n, err := memoryStoreReleaseAllUnder(worker); err != nil || n != 0 {
		t.Fatalf("released again %d %v", n, err)
	}
	if n, err := memoryStoreReleaseAllUnder(other); err != nil || n != 1 {
		t.Fatalf("other released %d %v", n, err)
	}
}

func TestMemoryStoreReleaseWithoutARunningHostIsZero(t *testing.T) {
	manageEnv(t)
	if hosts := plugins.RunningPluginHosts(); len(hosts) != 0 {
		t.Fatalf("hosts running %d", len(hosts))
	}
	if n, err := memoryStoreReleaseAllUnder(t.TempDir()); err != nil || n != 0 {
		t.Fatalf("released %d %v", n, err)
	}
}

func TestMemoryStoreReleaseReportsTheImportErrorWhenTheStoreIsNotInstalled(t *testing.T) {
	tmp := manageEnv(t)
	holographicHost(t)
	if err := os.RemoveAll(filepath.Join(tmp, ".icebath", "plugins", "holographic")); err != nil {
		t.Fatal(err)
	}
	var ie *memory.ProviderImportError
	n, err := memoryStoreReleaseAllUnder(tmp)
	if n != 0 || !errors.As(err, &ie) || ie.Msg != "memory provider 'holographic' is not installed" {
		t.Fatalf("released %d %v", n, err)
	}
}

func TestDeleteReleasesThisProcessMemoryStoreConnections(t *testing.T) {
	manageEnv(t)
	out := captureOut(t)
	dir := createWorker(t)
	holographicHost(t, filepath.Join(dir, "memory_store.db"))
	deleteQuietly(t, "worker")
	if !strings.Contains(out.String(), "✓ Released 1 memory-store connection(s) held by this process\n") {
		t.Fatalf("output %q", out.String())
	}
}
