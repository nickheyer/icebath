"""Secret redaction for plugin text, performed by Icebath's redactor."""

from __future__ import annotations

from typing import Any

from icebath_cli.plugin_sdk import parent_call


def redact_sensitive_text(text: str, *, force: bool = False, code_file: bool = False, **kwargs: Any) -> str:
    """Mask API keys, tokens and credentials in *text* the way Icebath's own output is masked."""
    return parent_call("redact_sensitive_text", text, force=force, code_file=code_file, **kwargs)
