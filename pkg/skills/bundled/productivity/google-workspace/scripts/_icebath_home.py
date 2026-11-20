"""Resolve ICEBATH_HOME for standalone skill scripts.

Skill scripts run as their own Python processes (started by the Icebath
``terminal`` tool, system Python, nix env, CI), apart from the Go binary
that owns the home.  This module provides ``get_icebath_home()`` and
``display_icebath_home()`` with the same rules as Icebath's ``pkg/home``
(``home.Dir`` and ``home.Display``), using only the stdlib.

Icebath exports ``ICEBATH_HOME`` to the processes it starts, profiles
included, so the variable wins; without it the platform default applies:
``%LOCALAPPDATA%\\icebath`` on Windows, ``~/.icebath`` elsewhere, each
followed by ``ICEBATH_DATA_DIR_SUFFIX`` when that is set.

All scripts under ``google-workspace/scripts/`` should import from here
instead of duplicating the ``ICEBATH_HOME = Path(os.getenv(...))`` pattern.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path


def get_icebath_home() -> Path:
    """Return the Icebath home directory in effect.

    Mirrors ``home.Dir`` in Icebath's ``pkg/home``."""
    val = os.environ.get("ICEBATH_HOME", "").strip()
    if val:
        return Path(os.path.expanduser(os.path.expandvars(val)))
    suffix = os.environ.get("ICEBATH_DATA_DIR_SUFFIX", "")
    if sys.platform == "win32":
        base = os.environ.get("LOCALAPPDATA", "").strip()
        root = Path(base) if base else Path.home() / "AppData" / "Local"
        return root / ("icebath" + suffix)
    return Path.home() / (".icebath" + suffix)


def display_icebath_home() -> str:
    """Return a user-friendly ``~/``-shortened display string.

    Mirrors ``home.Display`` in Icebath's ``pkg/home``."""
    home = get_icebath_home()
    try:
        return "~/" + home.relative_to(Path.home()).as_posix()
    except ValueError:
        return str(home)
