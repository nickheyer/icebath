"""Credential pool reads for plugin code, answered from Icebath's auth store."""

from __future__ import annotations

from typing import Any, Optional

from icebath_cli.plugin_sdk import parent_call


def read_credential_pool(provider_id: Optional[str] = None) -> dict[str, Any]:
    """The profile's credential pool rows for *provider_id*, or every pool when None."""
    return parent_call("read_credential_pool", provider_id)
