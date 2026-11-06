"""Plugin LLM facade types for plugins running in the plugin host; the host owns ``ctx.llm`` and its calls."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Optional


@dataclass
class PluginLlmTextInput:
    """Text block in a structured input list."""

    text: str
    type: str = "text"


@dataclass
class PluginLlmImageInput:
    """Image block: ``data`` (raw bytes) or ``url`` (http(s)/data: URL).
    ``mime_type`` is required for non-PNG bytes to render across providers."""

    data: Optional[bytes] = None
    url: Optional[str] = None
    mime_type: str = "image/png"
    file_name: str = ""
    type: str = "image"


PluginLlmInput = PluginLlmTextInput | PluginLlmImageInput | dict[str, Any]
"""One structured input block: a dataclass above or a plain dict of the same shape."""


@dataclass
class PluginLlmUsage:
    """Token + cost usage; every field optional. ``cost_usd`` is the host's best estimate."""

    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0
    cost_usd: Optional[float] = None


@dataclass
class PluginLlmCompleteResult:
    """Result of :meth:`PluginLlm.complete`."""

    text: str
    provider: str
    model: str
    agent_id: str
    usage: PluginLlmUsage = field(default_factory=PluginLlmUsage)
    audit: dict[str, Any] = field(default_factory=dict)


@dataclass
class PluginLlmStructuredResult:
    """Result of :meth:`PluginLlm.complete_structured`.

    ``parsed`` is set only when JSON output was requested AND the response was
    valid JSON; ``content_type`` is then ``"json"``, otherwise ``"text"``."""

    text: str
    provider: str
    model: str
    agent_id: str
    usage: PluginLlmUsage = field(default_factory=PluginLlmUsage)
    parsed: Optional[Any] = None
    content_type: str = "text"
    audit: dict[str, Any] = field(default_factory=dict)



class PluginLlmTrustError(PermissionError):
    """Raised when a plugin attempts an LLM override without trust."""


__all__ = [
    "PluginLlmCompleteResult",
    "PluginLlmImageInput",
    "PluginLlmInput",
    "PluginLlmStructuredResult",
    "PluginLlmTextInput",
    "PluginLlmTrustError",
    "PluginLlmUsage",
]
