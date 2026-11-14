package plugins

import ()

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
