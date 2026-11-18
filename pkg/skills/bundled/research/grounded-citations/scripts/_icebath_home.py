"""Resolve ICEBATH_HOME for standalone skill scripts.

Skill scripts run as their own Python processes (started by the Icebath
``terminal`` tool, system Python, nix env, CI), apart from the Go binary
that owns the home.  This module provides ``get_icebath_home()`` with the
same rules as ``home.Dir`` in Icebath's ``pkg/home``, using only the stdlib.

Icebath exports ``ICEBATH_HOME`` to the processes it starts, profiles
included, so the variable wins; without it the platform default applies:
``%LOCALAPPDATA%\\icebath`` on Windows, ``~/.icebath`` elsewhere, each
followed by ``ICEBATH_DATA_DIR_SUFFIX`` when that is set.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path


def get_icebath_home() -> Path:
    """Return the Icebath home directory in effect (default: ``~/.icebath``)."""
    val = os.environ.get("ICEBATH_HOME", "").strip()
    if val:
        return Path(os.path.expanduser(os.path.expandvars(val)))
    suffix = os.environ.get("ICEBATH_DATA_DIR_SUFFIX", "")
    if sys.platform == "win32":
        base = os.environ.get("LOCALAPPDATA", "").strip()
        root = Path(base) if base else Path.home() / "AppData" / "Local"
        return root / ("icebath" + suffix)
    return Path.home() / (".icebath" + suffix)
