"""Plugin SDK calls that run in Icebath, sent to it over the plugin host channel."""

from __future__ import annotations

from typing import Any, Optional

from icebath_cli.plugin_host_wire import PluginHostUnavailable, decode, encode

_RUNTIME: Optional[Any] = None


def bind_runtime(runtime: Any) -> None:
    """Route SDK calls through *runtime*'s channel (the serving host process)."""
    global _RUNTIME
    _RUNTIME = runtime


def parent_call(op: str, *args: Any, **kwargs: Any) -> Any:
    """Run SDK operation *op* in Icebath for the request being served and return its decoded result."""
    runtime = _RUNTIME
    if runtime is None:
        raise PluginHostUnavailable(f"plugin SDK call {op!r} needs the Icebath plugin host channel")
    return decode(runtime.channel.call("sdk", {"op": op, "args": encode(list(args)), "kwargs": encode(kwargs)}))
