"""Resolve ICEBATH_HOME for standalone skill scripts.

Skill scripts may run outside the Icebath process (e.g. system Python,
nix env, CI) where ``icebath_constants`` is not importable.  This module
provides the same ``get_icebath_home()`` and ``display_icebath_home()``
contracts as ``icebath_constants`` without requiring it on ``sys.path``.

When ``icebath_constants`` IS available it is used directly so that any
future enhancements (profile resolution, Docker detection, etc.) are
picked up automatically.  The fallback path replicates the core logic
from ``icebath_constants.py`` using only the stdlib.

All scripts under ``google-workspace/scripts/`` should import from here
instead of duplicating the ``ICEBATH_HOME = Path(os.getenv(...))`` pattern.
"""

from __future__ import annotations

import os
from pathlib import Path

try:
    from icebath_constants import display_icebath_home
    from icebath_constants import get_icebath_home
except (ModuleNotFoundError, ImportError):

    def get_icebath_home() -> Path:
        """Return the Icebath home directory (default: ~/.icebath).

        Mirrors ``icebath_constants.get_icebath_home()``."""
        val = os.environ.get("ICEBATH_HOME", "").strip()
        return Path(val) if val else Path.home() / ".icebath"

    def display_icebath_home() -> str:
        """Return a user-friendly ``~/``-shortened display string.

        Mirrors ``icebath_constants.display_icebath_home()``."""
        home = get_icebath_home()
        try:
            return "~/" + home.relative_to(Path.home()).as_posix()
        except ValueError:
            return str(home)
