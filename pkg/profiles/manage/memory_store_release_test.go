package manage

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
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

func TestMemoryStoreReleaseWithoutARunningHostIsZero(t *testing.T) {
	manageEnv(t)
	if hosts := plugins.RunningPluginHosts(); len(hosts) != 0 {
		t.Fatalf("hosts running %d", len(hosts))
	}
	if n, err := memoryStoreReleaseAllUnder(t.TempDir()); err != nil || n != 0 {
		t.Fatalf("released %d %v", n, err)
	}
}
