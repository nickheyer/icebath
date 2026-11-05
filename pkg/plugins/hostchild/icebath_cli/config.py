"""Configuration values plugin code reads, resolved by Icebath for this host's profile."""

from __future__ import annotations

from typing import Optional

from icebath_cli.plugin_sdk import parent_call


def get_env_value(key: str) -> Optional[str]:
    """*key* from the profile's ``.env`` or the process environment, None when unset."""
    return parent_call("get_env_value", key)
