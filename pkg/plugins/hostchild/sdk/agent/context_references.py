"""@-reference expansion (``@file:``, ``@folder:``, ``@diff``, ``@git:``, ``@url:`` + plugin prefixes)."""

from __future__ import annotations

import asyncio
import inspect
import json
import mimetypes
import os
import re
import subprocess
import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass, field
from pathlib import Path
from typing import Awaitable, Callable


# ── Plugin context-reference provider API ────────────────────────────────────

# --------------------------------------------------------------------------- Plugin context-reference
# provider API (Issue #26193) ---------------------------------------------------------------------------
BUILTIN_PREFIXES = frozenset({"diff", "staged", "file", "folder", "git", "url"})

_context_reference_providers: dict[str, ContextReferenceProvider] = {}


@dataclass(slots=True)
class ContextCompletionItem:
    """A single autocomplete result from a context reference provider (a dataclass, so it crosses
    the plugin-host wire as data)."""

    text: str
    display: str = ""
    meta: str = ""

    def __post_init__(self) -> None:
        self.display = self.display or self.text


class ContextReferenceProvider(ABC):
    """Base class for plugin @-prefix providers, registered via ``PluginContext.register_context_reference()``."""

    prefix: str = ""  # e.g. "issue", "channel", "doc"
    description: str = ""  # shown in autocomplete meta column

    @abstractmethod
    async def autocomplete(self, query: str, *, limit: int = 10) -> list[ContextCompletionItem]:
        """Return autocomplete items for the given query string."""

    @abstractmethod
    async def expand(self, target: str) -> str | None:
        """Expand *target* to prompt content.  Return ``None`` to skip."""


@dataclass(frozen=True)
class ContextReference:
    raw: str
    kind: str
    target: str
    start: int
    end: int
    line_start: int | None = None
    line_end: int | None = None


@dataclass
class ContextReferenceResult:
    message: str
    original_message: str
    references: list[ContextReference] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    injected_tokens: int = 0
    expanded: bool = False
    blocked: bool = False


UrlFetcher = Callable[[str], str | Awaitable[str]] | None
Expansion = tuple[str | None, str | None]  # (warning, block) — exactly one side is set
