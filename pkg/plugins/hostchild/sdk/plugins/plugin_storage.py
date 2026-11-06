"""Per-plugin persistent storage: ``<icebath home>/plugin-data/<name>/``.

Plugins must NOT park state in ``<icebath home>/plugins/<name>/`` (the install dir, deleted by
``remove`` and git-pulled by ``update``). Secrets are deliberately NOT part of this convention —
credential reads go through ``agent.secret_scope`` / ``.env``.
Usage: ``plugin_data_dir("my-plugin") / "state.json"``; ``plugin_db("my-plugin")`` → ``data.db``.
"""

from __future__ import annotations

import re
import sqlite3
from pathlib import Path

__all__ = ["plugin_data_dir", "plugin_db"]

# Mirrors the plugin-name shape `icebath plugins install` accepts (no separators/traversal).
_NAME_RE = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$")


def _validate_name(name: str) -> str:
    if not _NAME_RE.fullmatch(name) or ".." in name:
        raise ValueError(f"invalid plugin name for storage: {name!r}")
    return name


def plugin_data_dir(name: str) -> Path:
    """Return (and create) ``<icebath home>/plugin-data/<name>/``; resolves ``get_icebath_home()`` on
    every call so it follows the active profile — don't cache across profile switches."""
    from icebath_constants import get_icebath_home
    root = get_icebath_home() / "plugin-data" / _validate_name(name)
    root.mkdir(parents=True, exist_ok=True)
    return root


def plugin_db(name: str, filename: str = "data.db") -> sqlite3.Connection:
    """Open ``<data dir>/<filename>``. Icebath applies the shared journal-mode verdict for this file
    (WAL normally; DELETE on WAL-reset-bug builds or network FS), then this process connects with
    ``check_same_thread=False`` and foreign keys on — caller owns transactions."""
    if Path(filename).name != filename or not filename:
        raise ValueError(f"invalid plugin db filename: {filename!r}")
    _validate_name(name)
    from icebath_cli.plugin_sdk import parent_call

    path = parent_call("plugin_db_prepare", name, filename, list(sqlite3.sqlite_version_info))
    conn = sqlite3.connect(path, timeout=5.0, check_same_thread=False)
    try:
        conn.row_factory = None
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("PRAGMA foreign_keys=ON")
    except BaseException:
        conn.close()
        raise
    return conn
