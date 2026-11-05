"""Version of the Icebath process this plugin host serves."""

from __future__ import annotations

from types import SimpleNamespace

from icebath_cli.plugin_sdk import parent_call


def get_version_info() -> SimpleNamespace:
    """Icebath's version record (``base_version``, ``derived_version``, ``source`` ...)."""
    return SimpleNamespace(**parent_call("get_version_info"))
