package memory

import (
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
