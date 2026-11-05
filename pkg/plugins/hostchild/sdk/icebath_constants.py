"""Icebath home of the plugin host: the profile directory this host serves."""

from __future__ import annotations

import os
from contextvars import ContextVar, Token
from pathlib import Path
from typing import Optional


def get_icebath_home() -> Path:
    """The task's routed home, else ``ICEBATH_HOME`` as Icebath set it for this host, else ``~/.icebath``."""
    override = _HOME_OVERRIDE.get()
    if override is not None:
        return Path(override)
    val = os.environ.get("ICEBATH_HOME", "").strip()
    return Path(val) if val else Path.home() / ".icebath"


def display_icebath_home() -> str:
    """The home for user-facing messages, ``~/``-relative when it lives under the user's home."""
    home = get_icebath_home()
    try:
        return "~/" + str(home.relative_to(Path.home()))
    except ValueError:
        return str(home)


_HOME_OVERRIDE: ContextVar[Optional[str]] = ContextVar("_ICEBATH_HOME_OVERRIDE", default=None)
_PINNED_PROCESS_HOME: Optional[str] = None


def set_icebath_home_override(home: Optional[str | Path]) -> Token:
    """Route this task to another profile home; returns the reset token."""
    return _HOME_OVERRIDE.set(str(home) if home is not None else None)


def reset_icebath_home_override(token: Token) -> None:
    _HOME_OVERRIDE.reset(token)


def get_icebath_home_override() -> Optional[str]:
    """The task's routed home, or None when the task runs for the host's own profile."""
    return _HOME_OVERRIDE.get()


def pin_process_icebath_home(home: Optional[str | Path]) -> None:
    """Pin (or with None release) the home routed-profile checks compare against."""
    global _PINNED_PROCESS_HOME
    _PINNED_PROCESS_HOME = str(home) if home is not None else None


def process_icebath_home_is_pinned() -> bool:
    return _PINNED_PROCESS_HOME is not None


def get_routing_process_icebath_home() -> str:
    """The home this process serves: the pin when one is set, else the launch ``ICEBATH_HOME``."""
    if _PINNED_PROCESS_HOME is not None:
        return _PINNED_PROCESS_HOME
    val = os.environ.get("ICEBATH_HOME", "").strip()
    return val if val else str(Path.home() / ".icebath")


def icebath_home_key(home: str | Path) -> str:
    """Comparable identity of a home path: expanded, resolved and case-normalised."""
    return os.path.normcase(os.path.realpath(os.path.expanduser(str(home))))
