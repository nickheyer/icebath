"""Late-binding seam hosted dashboard plugin APIs use to reach the dashboard guard and state."""
from __future__ import annotations

import importlib
import sys
from typing import Any

WEB_SERVER = "icebath_cli.web_server"


def _server(module: str = WEB_SERVER):
    """The live module, imported on demand."""
    mod = sys.modules.get(module)
    if mod is None:
        mod = importlib.import_module(module)
    return mod


def late(name: str, module: str = WEB_SERVER):
    """Proxy that resolves a callable of the module at call time."""
    def _proxy(*args: Any, **kwargs: Any):
        return getattr(_server(module), name)(*args, **kwargs)
    _proxy.__name__ = name
    _proxy.__qualname__ = name
    return _proxy


class LateState:
    """Live proxy forwarding every access, comparison and with block to module state at operation time."""

    __slots__ = ("_module", "_name")

    def __init__(self, name: str, module: str = WEB_SERVER) -> None:
        object.__setattr__(self, "_name", name)
        object.__setattr__(self, "_module", module)

    def _target(self) -> Any:
        return getattr(_server(object.__getattribute__(self, "_module")),
                       object.__getattribute__(self, "_name"))

    def __getattr__(self, attr: str) -> Any:
        return getattr(self._target(), attr)

    def __getitem__(self, key: Any) -> Any:
        return self._target()[key]

    def __setitem__(self, key: Any, value: Any) -> None:
        self._target()[key] = value

    def __delitem__(self, key: Any) -> None:
        del self._target()[key]

    def __contains__(self, item: Any) -> bool:
        return item in self._target()

    def __iter__(self):
        return iter(self._target())

    def __len__(self) -> int:
        return len(self._target())

    def __bool__(self) -> bool:
        return bool(self._target())

    def __enter__(self):
        return self._target().__enter__()

    def __exit__(self, *exc):
        return self._target().__exit__(*exc)

    def __eq__(self, other: object) -> bool:
        return self._target() == other

    def __ne__(self, other: object) -> bool:
        return self._target() != other

    def __lt__(self, other: Any) -> bool:
        return self._target() < other

    def __le__(self, other: Any) -> bool:
        return self._target() <= other

    def __gt__(self, other: Any) -> bool:
        return self._target() > other

    def __ge__(self, other: Any) -> bool:
        return self._target() >= other

    def __hash__(self) -> int:
        return hash(self._target())

    def __repr__(self) -> str:
        return f"<LateState {object.__getattribute__(self, '_name')} -> {self._target()!r}>"
