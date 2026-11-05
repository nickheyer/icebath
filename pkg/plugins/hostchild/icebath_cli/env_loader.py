"""Provenance of the secrets Icebath placed in this plugin host's environment."""

from __future__ import annotations

from icebath_cli.plugin_sdk import parent_call


def format_secret_source_suffix(name: str) -> str:
    """`` (from <source>)`` naming the secret source that supplied env var *name*, ``""`` otherwise."""
    return parent_call("format_secret_source_suffix", name)
