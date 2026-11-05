"""Chat-capable model ids of a provider ``/models`` listing, filtered by Icebath's catalog rules."""

from __future__ import annotations

from typing import Any

from icebath_cli.plugin_sdk import parent_call


def chat_catalog_ids(items: Any) -> list[str]:
    return list(parent_call("chat_catalog_ids", items))
