"""Plugin module loading for category plugins inside the plugin host: per-module serialized loads
with a deadlock-aware bounded wait, synthetic packages, eager siblings, and instance extraction."""
from __future__ import annotations

import contextlib
import importlib
import importlib.machinery
import importlib.util
import logging
import os
import sys
import threading
import time
from contextvars import ContextVar
from pathlib import Path
from typing import Any, Callable, Optional

_log = logging.getLogger(__name__)

_PLUGINS_ROOT = Path(os.environ.get("ICEBATH_BUNDLED_PLUGINS") or Path(__file__).parent)
_MODULE_LOAD_LOCKS: dict[str, threading.RLock] = {}
_MODULE_LOAD_LOCKS_GUARD = threading.Lock()
_CONCURRENT_LOAD_WAIT_SECS = 10.0
_BOUNDED_WAIT: ContextVar[bool] = ContextVar("plugin_load_bounded_wait", default=False)
_STALLED_LOADS: set[str] = set()
_LOAD_OWNERS: dict[str, int] = {}
_LOAD_WAITERS: dict[int, str] = {}


def _waits_for(tid: int) -> set:
    """Threads *tid* is blocked on: a loader lock owner, or an importlib module-lock owner."""
    owners = {_LOAD_OWNERS.get(_LOAD_WAITERS.get(tid, ""))}
    blocked = getattr(importlib._bootstrap, "_blocking_on", {}).get(tid)
    owners.update(getattr(lk, "owner", None) for lk in (blocked if isinstance(blocked, list) else [blocked]))
    return owners - {None}


def _load_would_deadlock(me: int) -> bool:
    """Whether the wait graph of loader locks and importlib module locks leads back to *me*."""
    seen, todo = set(), list(_waits_for(me))
    while todo:
        tid = todo.pop()
        if tid == me:
            return True
        if tid not in seen:
            seen.add(tid)
            todo.extend(_waits_for(tid))
    return False


@contextlib.contextmanager
def bounded_load_wait():
    """Loads inside give up (None) after ``_CONCURRENT_LOAD_WAIT_SECS`` instead of waiting out another thread."""
    token = _BOUNDED_WAIT.set(True)
    try:
        yield
    finally:
        _BOUNDED_WAIT.reset(token)


def _module_load_lock(module_name: str) -> threading.RLock:
    with _MODULE_LOAD_LOCKS_GUARD:
        return _MODULE_LOAD_LOCKS.setdefault(module_name, threading.RLock())


def register_synthetic_package(name: str, search_locations: list[str]) -> None:
    """Register an empty package shell so ``<name>.<child>`` relative imports resolve."""
    if name in sys.modules:
        return
    spec = importlib.machinery.ModuleSpec(name, None, is_package=True)
    spec.submodule_search_locations = search_locations
    sys.modules[name] = importlib.util.module_from_spec(spec)


def user_plugins_dir() -> Optional[Path]:
    """Return ``$ICEBATH_HOME/plugins/`` or None if unavailable."""
    try:
        from icebath_constants import get_icebath_home
        d = get_icebath_home() / "plugins"
        return d if d.is_dir() else None
    except Exception:
        return None


def iter_plugin_dirs(root: Path) -> list[Path]:
    """Sorted child dirs of *root* that have an ``__init__.py`` (skips ``_``/``.`` names)."""
    if not root.is_dir():
        return []
    dirs: list[Path] = []
    for child in sorted(root.iterdir()):
        if child.name.startswith(("_", ".")):
            continue
        try:
            if child.is_dir() and (child / "__init__.py").exists():
                dirs.append(child)
        except OSError as exc:
            _log.warning("Skipping unreadable plugin directory %s: %s", child, exc)
    return dirs


def read_plugin_description(plugin_dir: Path) -> str:
    """Return ``description`` from ``plugin.yaml`` (empty string if absent/unreadable)."""
    try:
        from utils import fast_safe_load

        with open(plugin_dir / "plugin.yaml", encoding="utf-8-sig") as f:
            meta = fast_safe_load(f) or {}
        return meta.get("description", "")
    except Exception:
        return ""


def _new_module(name: str, file: Path, search_locations: Optional[list[str]] = None) -> Optional[Any]:
    """spec -> module -> sys.modules[name] (NOT executed); None if no spec."""
    spec = importlib.util.spec_from_file_location(
        name, str(file), submodule_search_locations=search_locations)
    if not spec:
        return None
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    return mod


def _exec(mod: Any, logger: Optional[logging.Logger] = None) -> bool:
    """Exec a ``_new_module`` module (None -> False); False plus a debug line when it raised."""
    if mod is None:
        return False
    try:
        mod.__spec__.loader.exec_module(mod)
        return True
    except Exception as e:
        if logger:
            logger.debug("Failed to exec_module %s: %s", mod.__name__, e)
        return False


def load_plugin_module(module_name: str, plugin_dir: Path, *, parents: tuple[str, ...],
                       logger: logging.Logger, synthetic_namespace: Optional[str] = None,
                       plugins_root: Optional[Path] = None) -> Optional[Any]:
    """Import ``plugin_dir/__init__.py`` as *module_name*: parents, then siblings, then the module, serialized per name."""
    init_file = plugin_dir / "__init__.py"
    if not init_file.exists():
        return None
    lock, me = _module_load_lock(module_name), threading.get_ident()
    deadline = float("inf") if not _BOUNDED_WAIT.get() else time.monotonic() + (
        0 if module_name in _STALLED_LOADS else _CONCURRENT_LOAD_WAIT_SECS)
    _LOAD_WAITERS[me] = module_name
    try:
        while not lock.acquire(timeout=0.05):
            if _load_would_deadlock(me):
                logger.debug("Concurrent circular load of %s; using the partial module", module_name)
                return sys.modules.get(module_name)
            if time.monotonic() >= deadline:
                _STALLED_LOADS.add(module_name)
                logger.warning("Skipping plugin %s: another thread's load of it has not finished "
                               "(import still running after %.0fs)", module_name, _CONCURRENT_LOAD_WAIT_SECS)
                return None
    finally:
        _LOAD_WAITERS.pop(me, None)
    outermost = module_name not in _LOAD_OWNERS
    _LOAD_OWNERS.setdefault(module_name, me)
    try:
        return _load_plugin_module_locked(module_name, plugin_dir, init_file, parents, logger,
                                          synthetic_namespace, plugins_root or _PLUGINS_ROOT)
    finally:
        if outermost:
            _LOAD_OWNERS.pop(module_name, None)
            _STALLED_LOADS.discard(module_name)
        lock.release()


def _load_plugin_module_locked(module_name: str, plugin_dir: Path, init_file: Path,
                               parents: tuple[str, ...], logger: logging.Logger,
                               synthetic_namespace: Optional[str], plugins_root: Path) -> Optional[Any]:
    cached = sys.modules.get(module_name)
    if cached is not None and getattr(cached, "__file__", None):
        return cached
    for parent in parents:
        parent_path = plugins_root.joinpath(*parent.split(".")[1:])
        if parent not in sys.modules and (parent_path / "__init__.py").exists():
            _exec(_new_module(parent, parent_path / "__init__.py", [str(parent_path)]))
    if synthetic_namespace:
        register_synthetic_package(synthetic_namespace, [])
    mod = _new_module(module_name, init_file, [str(plugin_dir)])
    if mod is None:
        return None
    loaded_submodules = []
    for sub_file in plugin_dir.glob("*.py"):
        full_sub_name = f"{module_name}.{sub_file.stem}"
        if sub_file.name == "__init__.py" or full_sub_name in sys.modules:
            continue
        sub_mod = _new_module(full_sub_name, sub_file)
        if _exec(sub_mod, logger):
            loaded_submodules.append((sub_file.stem, sub_mod))
        else:
            sys.modules.pop(full_sub_name, None)
    if not _exec(mod, logger):
        sys.modules.pop(module_name, None)
        return None
    parent_name, child_name = module_name.rsplit(".", 1)
    parent_mod = sys.modules.get(parent_name)
    if parent_mod is not None:
        setattr(parent_mod, child_name, mod)
    for sub_name, sub_mod in loaded_submodules:
        setattr(mod, sub_name, sub_mod)
    return mod


class NoopPluginContext:
    """Base for fake ``register(ctx)`` contexts: no-op registrations except the one a subclass overrides."""

    def _noop(self, *args, **kwargs):
        pass

    register_tool = register_hook = register_cli_command = register_memory_provider = _noop


def instance_from_module(mod: Any, *, collector: Any, collected_attr: str, base_cls: type, name: str,
                         logger: logging.Logger) -> Optional[Any]:
    """Extract the provider instance: ``register(ctx)`` first, then any ``base_cls`` subclass."""
    if hasattr(mod, "register"):
        try:
            mod.register(collector)
            instance = getattr(collector, collected_attr)
            if instance:
                return instance
        except Exception as e:
            logger.debug("register() failed for %s: %s", name, e)
    for attr_name in dir(mod):
        attr = getattr(mod, attr_name, None)
        if isinstance(attr, type) and issubclass(attr, base_cls) and attr is not base_cls:
            with contextlib.suppress(Exception):
                return attr()
    return None


def load_named(name: str, plugin_dir: Path, load_from_dir: Callable[[Path], Optional[Any]], *, kind: str,
               noun: str, logger: logging.Logger) -> Optional[Any]:
    """Load from *plugin_dir*, a warning and None on failure or when no instance came back."""
    try:
        instance = load_from_dir(plugin_dir)
    except Exception as e:
        logger.warning("Failed to load %s '%s': %s", kind.lower(), name, e)
        return None
    if not instance:
        logger.warning("%s '%s' loaded but no %s instance found", kind, name, noun)
    return instance or None


def probe_availability(load: Callable[[], Optional[Any]]) -> bool:
    """True iff *load()* returns an instance whose ``is_available()`` (if any) is truthy."""
    try:
        instance = load()
        return instance is not None and (instance.is_available() if hasattr(instance, "is_available") else True)
    except Exception:
        return False
