"""Resolve ICEBATH_HOME for standalone skill scripts.

Skill scripts may run outside the Icebath process (system Python, nix env,
CI) where ``icebath_constants`` is not importable.  This module provides the
same ``get_icebath_home()`` contract without requiring it on ``sys.path``.

When ``icebath_constants`` IS available it is used directly so profile
resolution and any future enhancements are picked up automatically.
"""

from __future__ import annotations

import os
from pathlib import Path

try:
    from icebath_constants import get_icebath_home
except (ModuleNotFoundError, ImportError):

    def get_icebath_home() -> Path:
        """Return the Icebath home directory (default: ``~/.icebath``)."""
        val = os.environ.get("ICEBATH_HOME", "").strip()
        return Path(val) if val else Path.home() / ".icebath"
