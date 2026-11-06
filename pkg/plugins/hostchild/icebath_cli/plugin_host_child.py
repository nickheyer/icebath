"""The plugin host process: imports third-party plugins and serves their callbacks to Icebath.

Started by :class:`icebath_cli.plugin_host.PluginHost` as ``python -m icebath_cli.plugin_host_child``
(optionally under ``plugins.host.launcher``, e.g. a sandbox runner) with the profile's environment.
Plugins get a :class:`RemotePluginContext` that looks like ``PluginContext``: registrations and
queries are forwarded to Icebath, callables and provider objects stay here and are invoked by
reference. Nothing in this module is imported by the Icebath process itself.

stdin/stdout carry the protocol, so the real file descriptors are duplicated for the channel and
fd 0/1 are pointed at /dev/null and stderr before any plugin code runs: a stray ``print()`` in a
plugin lands in the host log instead of corrupting the wire.
"""

from __future__ import annotations

import asyncio
import functools
import importlib
import importlib.machinery
import importlib.metadata
import importlib.util
import inspect
import itertools
import logging
import os
import sys
import threading
import types
import weakref
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional

from icebath_cli.plugin_host_wire import (
    Channel, Opaque, PROTOCOL_VERSION, PluginHostUnavailable, PluginHostUnsupported, bind_serving_request,
    declared_member, decode,
    describe_signature, encode, is_async_callable, restore_arguments, serving_request,
)

logger = logging.getLogger("icebath_cli.plugin_host_child")

_ENTRY_POINTS_GROUP = "icebath_agent.plugins"
_MEMORY_ENTRY_POINTS_GROUP = "icebath_agent.memory_providers"
_USER_MEMORY_NAMESPACE = "_icebath_user_memory"


class RemoteRegistration:
    """Child-side twin of ``PluginRegistration``: ``dispose()`` releases the Icebath-side entry."""

    def __init__(self, runtime: HostRuntime, handle_id: int, kind: str, key: str):
        self._runtime, self._id, self.kind, self.key = runtime, handle_id, kind, key
        self._disposed = False

    @property
    def active(self) -> bool:
        return not self._disposed

    def dispose(self) -> None:
        if self._disposed:
            return
        self._disposed = True
        self._runtime.channel.call("dispose", {"handle": self._id})


class RemoteFacade:
    """``ctx.state`` / ``ctx.llm`` / ...: every method call runs on the Icebath-side facade."""

    def __init__(self, runtime: HostRuntime, plugin_key: str, name: str):
        self._runtime, self._plugin_key, self._name = runtime, plugin_key, name
        self._methods: dict[str, bool] = {}  # method name -> is a coroutine function in Icebath

    def __getattr__(self, attr: str) -> Any:
        if attr.startswith("_"):
            raise AttributeError(attr)
        if attr not in self._methods:
            value = self._runtime.facade_call(self._plugin_key, self._name, attr, _probe=True)
            if not (isinstance(value, dict) and value.get("__method__")):
                return value  # a plain attribute (``ctx.state.data_dir``): read fresh every time
            self._methods[attr] = bool(value.get("async"))
        call = functools.partial(self._runtime.facade_call, self._plugin_key, self._name, attr)
        if not self._methods[attr]:
            return call

        async def remote(*args: Any, **kwargs: Any) -> Any:  # ``await ctx.llm.acomplete(...)``
            return await asyncio.to_thread(call, *args, **kwargs)

        remote.__name__ = attr
        return remote


class ParentCallable:
    """An Icebath callable passed into the host: calling it runs the Icebath function and returns its answer."""

    def __init__(self, runtime: HostRuntime, parent_id: int, owner: str):
        self._runtime, self._id, self._owner = runtime, parent_id, owner
        weakref.finalize(self, runtime.release_parent, parent_id)

    def __call__(self, *args: Any, **kwargs: Any) -> Any:
        return self._runtime.parent_call(self._id, self._owner, args, kwargs)

    def __repr__(self) -> str:
        return f"<icebath callable {self._id}>"


class ParentEvent(threading.Event):
    """An Icebath event passed into the host: is_set, wait, set and clear act on the Icebath flag.

    Icebath keeps one id per event while it holds the event, so no release is sent. Once the Icebath
    channel is gone the event reads as set, so loops waiting on it stop."""

    def __init__(self, runtime: HostRuntime, parent_id: int):
        super().__init__()
        self._runtime, self._id = runtime, parent_id

    def __icebath_ref__(self) -> dict:
        return {"__parent_event__": self._id}

    def _op(self, op: str, timeout: Optional[float] = None) -> Any:
        try:
            return self._runtime.parent_event(self._id, op, timeout)
        except PluginHostUnavailable:
            return op in ("is_set", "wait")

    def is_set(self) -> bool:
        return bool(self._op("is_set"))

    isSet = is_set

    def set(self) -> None:
        self._op("set")

    def clear(self) -> None:
        self._op("clear")

    def wait(self, timeout: Optional[float] = None) -> bool:
        return bool(self._op("wait", timeout))

    def __repr__(self) -> str:
        return f"<icebath event {self._id}: {'set' if self.is_set() else 'unset'}>"


class RemotePluginContext:
    """What ``register(ctx)`` receives inside the host. Mirrors ``PluginContext``'s public surface as
    announced by Icebath at load, so ``hasattr(ctx, "register_x")`` answers the same as in-process."""

    def __init__(self, runtime: HostRuntime, plugin_key: str, info: dict[str, Any]):
        self._runtime, self._plugin_key = runtime, plugin_key
        self._methods = set(info.get("ctx_methods") or ())
        self._facades: dict[str, RemoteFacade] = {}
        self.manifest = types.SimpleNamespace(**(info.get("manifest") or {}))
        self.plugin_id = info.get("plugin_id") or plugin_key
        self.profile_name = info.get("profile_name")

    def __getattr__(self, name: str) -> Any:
        if name.startswith("_"):
            raise AttributeError(name)
        from icebath_cli.plugin_isolation import (
            HOST_REMOTE_FACADES, HOST_SKIPPED_CTX_METHODS, HOST_UNSUPPORTED_CTX_METHODS,
        )
        if name in HOST_UNSUPPORTED_CTX_METHODS:
            def unsupported(*_args: Any, **_kwargs: Any) -> Any:
                raise PluginHostUnsupported(
                    f"ctx.{name}() cannot run in the plugin host: {HOST_UNSUPPORTED_CTX_METHODS[name]}")
            return unsupported
        if name in HOST_SKIPPED_CTX_METHODS:
            def skipped(*_args: Any, **_kwargs: Any) -> None:
                logger.warning("Plugin '%s': ctx.%s() is skipped in the plugin host (%s)",
                               self.manifest.name, name, HOST_SKIPPED_CTX_METHODS[name])
            return skipped
        if name in HOST_REMOTE_FACADES:
            return self._facades.setdefault(name, RemoteFacade(self._runtime, self._plugin_key, name))
        if name not in self._methods:
            raise AttributeError(f"'PluginContext' object has no attribute {name!r}")
        return functools.partial(self._runtime.ctx_call, self._plugin_key, name)

    def on_unload(self, callback: Callable[[], Any]) -> None:
        if not callable(callback):
            raise TypeError("on_unload() callback must be callable")
        self._runtime.unload_callbacks.setdefault(self._plugin_key, []).append(callback)

    def spawn_task(self, coro: Any, *, name: Optional[str] = None) -> Any:
        return self._runtime.spawn(coro, name=name)


class HostRuntime:
    """Reference tables, the asyncio loop for async plugin code, and the request handler."""

    def __init__(self, reader, writer):
        self.refs: dict[int, Any] = {}
        self.owners: dict[int, str] = {}
        self._ids = itertools.count(1)
        self._lock = threading.Lock()
        self.unload_callbacks: dict[str, list[Callable[[], Any]]] = {}
        self.modules: dict[str, str] = {}
        self.asgi_apps: dict[str, Any] = {}
        self.profiles: dict[str, dict[str, Any]] = {}
        self.instance_modules: dict[str, Any] = {}
        self.entry_points: dict[str, Any] = {}
        self.stopped = threading.Event()
        self._released_parents: list[int] = []
        self.loop = asyncio.new_event_loop()
        threading.Thread(target=self.loop.run_forever, name="plugin-host-loop", daemon=True).start()
        self.channel = Channel(reader, writer, self.handle, name="plugin-host",
                               on_close=lambda _reason: self.stopped.set())
        from icebath_cli import plugin_sdk
        plugin_sdk.bind_runtime(self)

    # -- references -----------------------------------------------------------------------------
    def _remember(self, plugin_key: str, value: Any) -> int:
        with self._lock:
            ref = next(self._ids)
            self.refs[ref] = value
            self.owners[ref] = plugin_key
        return ref

    def ref_for(self, plugin_key: str, value: Any, *, allow_objects: bool) -> Optional[dict]:
        if isinstance(value, type):
            return None
        if isinstance(value, threading.Event):
            return self.event_ref(value, plugin_key)
        if callable(value) and not (allow_objects and _is_provider_object(value)):
            return {"__callable__": self._remember(plugin_key, value), "async": is_async_callable(value),
                    "sig": describe_signature(value), "name": getattr(value, "__name__", "callback"),
                    "qualname": getattr(value, "__qualname__", None),
                    "module": getattr(value, "__module__", None)}
        if allow_objects and _is_provider_object(value):
            return self._describe_object(plugin_key, value)
        return None

    def _describe_object(self, plugin_key: str, obj: Any) -> dict:
        """Method table + attribute plan for a provider object; every method runs here, in the host."""
        methods: dict[str, Any] = {}
        instance = sorted(k for k in vars(obj) if not k.startswith("_")) if hasattr(obj, "__dict__") else []
        live: list[str] = list(instance)
        for name in dir(obj):
            if name.startswith("_") or name in instance:
                continue
            raw = inspect.getattr_static(obj, name, None)
            if isinstance(raw, property) or type(raw).__name__ == "cached_property":
                live.append(name)
                continue
            value = getattr(obj, name, None)
            if callable(value):
                methods[name] = {"async": is_async_callable(value), "sig": describe_signature(value)}
            else:
                # Class-level defaults too: an engine that sets ``context_length`` only later in
                # ``update_model()`` must not read as the base-class default forever in Icebath.
                live.append(name)
        return {"__object__": self._remember(plugin_key, obj), "methods": methods, "live": live,
                "type": type(obj).__name__}

    def event_ref(self, value: Any, plugin_key: str = "") -> Optional[dict]:
        """Wire ref of a threading.Event: an Icebath event keeps its id, a host event is remembered here."""
        if not isinstance(value, threading.Event):
            return None
        if isinstance(value, ParentEvent):
            return value.__icebath_ref__()
        return {"__child_event__": self._remember(plugin_key, value)}

    def resolve_event(self, ref: dict) -> Any:
        if "__parent_event__" in ref:
            return ParentEvent(self, int(ref["__parent_event__"]))
        event = self.refs.get(int(ref["__child_event__"]))
        if not isinstance(event, threading.Event):
            raise LookupError(f"plugin host event {int(ref['__child_event__'])} was released")
        return event

    def resolve_from_parent(self, ref: dict) -> Any:
        if "__parent_event__" in ref or "__child_event__" in ref:
            return self.resolve_event(ref)
        if "__parent_callable__" in ref:
            return ParentCallable(self, int(ref["__parent_callable__"]), "")
        if "__handle__" in ref:
            return RemoteRegistration(self, int(ref["__handle__"]), str(ref.get("kind") or ""),
                                      str(ref.get("key") or ""))
        return Opaque("callable" if "__callable__" in ref else "object")

    # -- outgoing ---------------------------------------------------------------------------------
    def release_parent(self, parent_id: int) -> None:
        with self._lock:
            self._released_parents.append(parent_id)

    def _flush_parent_releases(self) -> None:
        """Tell Icebath which of its callables the host no longer holds, so it can forget them."""
        with self._lock:
            ids, self._released_parents = self._released_parents, []
        if ids:
            try:
                self.channel.call("release_parent", {"ids": ids})
            except PluginHostUnavailable:
                pass

    def parent_event(self, parent_id: int, op: str, timeout: Optional[float] = None) -> Any:
        self._flush_parent_releases()
        return self.channel.call("parent_event", {"id": parent_id, "op": op, "timeout": timeout})

    def parent_call(self, parent_id: int, owner: str, args: tuple, kwargs: dict) -> Any:
        self._flush_parent_releases()
        refs = functools.partial(self.ref_for, owner, allow_objects=False)
        return decode(self.channel.call("parent_call", {"id": parent_id, "args": encode(list(args), refs),
                                                        "kwargs": encode(dict(kwargs), refs)}),
                      self.resolve_from_parent)

    def _send_call(self, method: str, plugin_key: str, payload: dict[str, Any], *, allow_objects: bool) -> Any:
        self._flush_parent_releases()
        refs = functools.partial(self.ref_for, plugin_key, allow_objects=allow_objects)
        payload = {**payload, "plugin": plugin_key,
                   "args": encode(list(payload.pop("args", ())), refs),
                   "kwargs": encode(dict(payload.pop("kwargs", {})), refs)}
        return decode(self.channel.call(method, payload), self.resolve_from_parent)

    def ctx_call(self, plugin_key: str, method: str, *args: Any, **kwargs: Any) -> Any:
        from icebath_cli.plugin_isolation import HOST_OBJECT_BASES
        return self._send_call("ctx", plugin_key, {"method": method, "args": args, "kwargs": kwargs},
                               allow_objects=method in HOST_OBJECT_BASES)

    def facade_call(self, plugin_key: str, facade: str, method: str, *args: Any, _probe: bool = False,
                    **kwargs: Any) -> Any:
        return self._send_call("facade", plugin_key, {"facade": facade, "method": method, "probe": _probe,
                                                      "args": args, "kwargs": kwargs}, allow_objects=False)

    def spawn(self, coro: Any, *, name: Optional[str] = None) -> Any:
        if not inspect.iscoroutine(coro):
            raise TypeError("spawn_task() requires a coroutine object")
        return asyncio.run_coroutine_threadsafe(_on_behalf_of(coro, serving_request()), self.loop)

    # -- incoming ---------------------------------------------------------------------------------
    def handle(self, method: str, params: dict[str, Any], _origin: Optional[int]) -> Any:
        handler = _HANDLERS.get(method)
        if handler is None:
            raise ValueError(f"unknown plugin host request {method!r}")
        try:
            return handler(self, params)
        finally:
            self._flush_parent_releases()

    def _run(self, result: Any) -> Any:
        """Await plugin coroutines on the host loop, still attributed to the request being served
        (their ``ctx`` calls then run in that caller's session, not a bare context)."""
        if inspect.isawaitable(result):
            return asyncio.run_coroutine_threadsafe(_on_behalf_of(result, serving_request()), self.loop).result()
        return result

    def _encode_result(self, plugin_key: str, value: Any) -> Any:
        def refs(obj: Any) -> Optional[dict]:
            if plugin_key.startswith(_PROFILE_CLIENT_OWNER) and _is_live_client_part(obj):
                return self._describe_object(plugin_key, obj)
            if _provider_base(obj) is not None:  # e.g. the engine ``clone_for_agent`` returns
                return self._describe_object(plugin_key, obj)
            return self.ref_for(plugin_key, obj, allow_objects=False)

        return encode(value, refs)

    def _decode_args(self, params: dict[str, Any], owner: str = ""):
        def resolve(ref: dict) -> Any:
            if "__parent_event__" in ref or "__child_event__" in ref:
                return self.resolve_event(ref)
            if "__parent_callable__" in ref:
                return ParentCallable(self, int(ref["__parent_callable__"]), owner)
            return {k: decode(v, resolve) for k, v in ref.items()}

        return decode(params.get("args") or [], resolve), decode(params.get("kwargs") or {}, resolve)

    def op_hello(self, _params: dict[str, Any]) -> dict[str, Any]:
        return {"protocol": PROTOCOL_VERSION, "pid": os.getpid(), "python": sys.version.split()[0]}

    def op_load(self, params: dict[str, Any]) -> dict[str, Any]:
        plugin_key = str(params["plugin_key"])
        target = _import_plugin(params)
        register = target if callable(target) and not isinstance(target, types.ModuleType) \
            else getattr(target, "register", None)
        if not callable(register):
            raise AttributeError(f"Plugin '{params.get('name')}' has no register() function")
        module_name = getattr(target, "__name__", None) or getattr(register, "__module__", None)
        self.modules[plugin_key] = str(module_name or "")
        self._run(register(RemotePluginContext(self, plugin_key, params)))
        module = target
        if target is not None and not isinstance(target, types.ModuleType) and callable(target):
            module = sys.modules.get(getattr(target, "__module__", ""))
        return {"module": module_name, "file": getattr(module, "__file__", None)}

    def op_load_tools(self, params: dict[str, Any]) -> dict[str, Any]:
        """A deferred platform plugin's ``tools.py``: import the package, then run ``register_tools(ctx)``."""
        plugin_key = str(params["plugin_key"])
        package = _import_plugin(params)
        self.modules[plugin_key] = package.__name__
        tools_module = importlib.import_module(f"{package.__name__}.tools")
        register_tools = getattr(tools_module, "register_tools", None)
        if register_tools is None:
            return {"module": package.__name__, "register_tools": False, "file": getattr(package, "__file__", None)}
        self._run(register_tools(RemotePluginContext(self, plugin_key, params)))
        return {"module": package.__name__, "register_tools": True, "file": getattr(package, "__file__", None)}

    def op_load_instance(self, params: dict[str, Any]) -> Any:
        """Category plugins (memory provider, context engine, cron scheduler): import the directory,
        run ``register(ctx)`` capturing the one ``capture`` registration, else instantiate the first
        subclass of ``base``. Other registrations reach Icebath only when it bound a ctx for them."""
        plugin_key = str(params["plugin_key"])
        if params.get("in_process"):
            return self._load_bundled_instance(plugin_key, params)
        # Icebath asks for a fresh instance many times (agent cache, doctor, dashboard); like the
        # in-process loader, the module body runs once per host and only the instance is new.
        module = self.instance_modules.get(str(params["path"]))
        if module is None:
            module = self.instance_modules[str(params["path"])] = _import_plugin(params)
        self.modules[plugin_key] = module.__name__
        base = _import_base(str(params["base"]))
        capture = str(params["capture"])
        captured: list[Any] = []
        register = getattr(module, "register", None)
        if callable(register):
            ctx = _CapturingContext(self, plugin_key, params, capture, base, captured,
                                    forward=bool(params.get("forward")))
            try:
                self._run(register(ctx))
            except Exception as exc:
                if not captured:
                    logger.debug("register() failed for %s: %s", params.get("name"), exc)
        if not captured:
            for attr in dir(module):
                value = getattr(module, attr, None)
                if isinstance(value, type) and issubclass(value, base) and value is not base:
                    try:
                        captured.append(value())
                        break
                    except Exception:
                        continue
        if not captured:
            return None
        return self._describe_object(plugin_key, captured[0])

    def _load_bundled_instance(self, plugin_key: str, params: dict[str, Any]) -> Any:
        """A bundled category plugin the way the in-process loader takes it: load_plugin_module under its
        parents, then instance_from_module over a collector whose other registrations reach Icebath."""
        from plugins import plugin_loader

        module = plugin_loader.load_plugin_module(
            str(params["module_name"]), Path(str(params["path"])), parents=tuple(params.get("parents") or ()),
            logger=logger, plugins_root=Path(str(params["plugins_root"])))
        if module is None:
            return None
        self.modules[plugin_key] = module.__name__
        base = _import_base(str(params["base"]))
        collector = _CollectorContext(self, plugin_key, params, str(params["capture"]))
        instance = plugin_loader.instance_from_module(module, collector=collector, collected_attr="collected",
                                                      base_cls=base, name=str(params.get("name")), logger=logger)
        return None if instance is None else self._describe_object(plugin_key, instance)

    def op_load_memory_entry_point(self, params: dict[str, Any]) -> Any:
        """A pip memory provider entry point: an instance, a subclass, a module with ``register(ctx)``, a factory or
        ``register`` callable, or a namespace holding a subclass, in that order; other registrations reach Icebath."""
        plugin_key, name = str(params["plugin_key"]), str(params["name"])
        entry_point = _find_entry_point(_MEMORY_ENTRY_POINTS_GROUP, name)
        if entry_point is None:
            raise ImportError(f"Entry point '{name}' not found in group '{_MEMORY_ENTRY_POINTS_GROUP}'")
        loaded = entry_point.load()
        self.modules[plugin_key] = entry_point.module
        provider = _entry_point_provider(self, plugin_key, params, loaded, _import_base(str(params["base"])), name)
        if provider is None:
            return None
        return self._describe_object(plugin_key, provider)

    def op_warm_import(self, params: dict[str, Any]) -> bool:
        """Import a memory provider package (or its entry point) without constructing it, then each native warm module."""
        name = str(params["name"])
        imported = False
        try:
            if params.get("path"):
                imported = _load_provider_package(str(params["module_name"]), Path(str(params["path"])),
                                                  [str(p) for p in params.get("parents") or []],
                                                  Path(str(params["plugins_root"])),
                                                  params.get("synthetic_namespace")) is not None
            elif (entry_point := _find_entry_point(_MEMORY_ENTRY_POINTS_GROUP, name)) is not None:
                entry_point.load()
                imported = True
        except Exception:
            logger.debug("memory provider '%s' warm-up import failed", name, exc_info=True)
        if imported:
            for module in params.get("native_warm") or ():
                try:
                    importlib.import_module(str(module))
                except Exception:
                    logger.debug("warm-up import of %s skipped", module, exc_info=True)
        return imported

    def op_describe_memory_cli(self, params: dict[str, Any]) -> dict[str, Any]:
        """Import a user memory provider's ``cli.py`` without its package ``__init__``; report its two hooks."""
        cli_mod = _import_memory_cli(str(params["module_name"]), Path(str(params["path"])))
        return {"register_cli": callable(getattr(cli_mod, "register_cli", None)),
                "handler": getattr(cli_mod, f"{params['name']}_command", None) is not None}

    def op_profile_call(self, params: dict[str, Any]) -> Any:
        """Run one overridden method (or callable field) of a model-provider profile, loading the
        plugin on first use in this host process. Addressed by name, so it survives host restarts."""
        target = getattr(self._profiles_for(params)[str(params["profile"])], str(params["attr"]))
        args, kwargs = self._decode_args(params)
        return encode(self._run(target(*args, **kwargs)))

    def _profiles_for(self, params: dict[str, Any]) -> dict[str, Any]:
        """Profiles of a plugin directory or provider entry point, captured again after a host restart."""
        path = str(params["path"])
        profiles = self.profiles.get(path)
        if profiles is None:
            if path.startswith(_ENTRY_POINT_PROFILE_KEY):
                captured = self._capture_entry_point(path[len(_ENTRY_POINT_PROFILE_KEY):], str(params["module_name"]))
            else:
                captured = capture_profiles(path, str(params["module_name"]))
            profiles = self.profiles[path] = {p.name: p for p in captured}
        return profiles

    def _capture_entry_point(self, name: str, value: str) -> list[Any]:
        """Profiles a provider entry point registers on load plus on its zero argument call, like discovery."""
        with _capturing() as captured:
            loaded = importlib.metadata.EntryPoint(name=name, value=value, group=_ENTRY_POINTS_GROUP).load()
            if callable(loaded) and not _requires_arguments(loaded):
                self._run(loaded())
        return captured

    def op_profile_client(self, params: dict[str, Any]) -> Any:
        """Call a profile's overridden ``create_client`` and keep the live SDK client here behind an object ref."""
        profile = self._profiles_for(params)[str(params["profile"])]
        args, kwargs = self._decode_args(params)
        client = self._run(profile.create_client(*args, **kwargs))
        return self._describe_object(f"{_PROFILE_CLIENT_OWNER}{params['path']}:{params['profile']}", client)

    def op_provider_entry_point_load(self, params: dict[str, Any]) -> Any:
        """``ep.load()`` of a provider entry point, reporting the profiles it registered and its target shape."""
        name, value = str(params["name"]), str(params["value"])
        key = _ENTRY_POINT_PROFILE_KEY + name
        reply: dict[str, Any] = {"callable": False, "requires_arguments": False}
        with _capturing() as captured:
            try:
                loaded = importlib.metadata.EntryPoint(name=name, value=value, group=_ENTRY_POINTS_GROUP).load()
            except Exception as exc:
                reply["error"] = {"type": type(exc).__name__, "message": str(exc)}
            else:
                self.entry_points[key] = loaded
                reply["callable"] = callable(loaded)
                reply["requires_arguments"] = callable(loaded) and _requires_arguments(loaded)
        self.profiles.setdefault(key, {}).update({p.name: p for p in captured})
        reply["profiles"] = [_extraction_entry(p) for p in captured]
        return reply

    def op_provider_entry_point_invoke(self, params: dict[str, Any]) -> Any:
        """The zero argument call of a loaded provider entry point, reporting the profiles it registered."""
        name, value = str(params["name"]), str(params["value"])
        key = _ENTRY_POINT_PROFILE_KEY + name
        loaded = self.entry_points.get(key)
        if loaded is None:
            loaded = self.entry_points[key] = importlib.metadata.EntryPoint(
                name=name, value=value, group=_ENTRY_POINTS_GROUP).load()
        reply: dict[str, Any] = {}
        with _capturing() as captured:
            try:
                self._run(loaded())
            except Exception as exc:
                reply["error"] = {"type": type(exc).__name__, "message": str(exc)}
        self.profiles.setdefault(key, {}).update({p.name: p for p in captured})
        reply["profiles"] = [_extraction_entry(p) for p in captured]
        return reply

    def op_provider_module_call(self, params: dict[str, Any]) -> Any:
        """Load a provider package the way the memory loader names it, then call one function of a submodule."""
        package = _load_provider_package(str(params["module_name"]), Path(str(params["path"])),
                                         [str(p) for p in params.get("parents") or []],
                                         Path(str(params["plugins_root"])), params.get("synthetic_namespace"))
        if package is None:
            return encode({"loaded": False})
        submodule = str(params.get("submodule") or "")
        target = importlib.import_module(f"{package.__name__}.{submodule}") if submodule else package
        function = getattr(target, str(params["function"]))
        args, kwargs = self._decode_args(params)
        return encode({"loaded": True, "result": self._run(function(*args, **kwargs))})

    def op_asgi(self, params: dict[str, Any]) -> Any:
        """Serve one dashboard API request with the plugin's FastAPI ``router`` (loaded once)."""
        api_path = Path(str(params["dashboard_dir"])) / str(params["api_file"])
        app = self.asgi_apps.get(str(api_path))
        if app is None:
            from fastapi import FastAPI
            module_name = f"icebath_dashboard_plugin_{params['plugin']}"
            spec = importlib.util.spec_from_file_location(module_name, api_path)
            if spec is None or spec.loader is None:
                raise ImportError(f"cannot load dashboard api {api_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[module_name] = module
            spec.loader.exec_module(module)
            router = getattr(module, "router", None)
            if router is None:
                raise AttributeError(f"dashboard api {api_path.name} has no 'router'")
            app = FastAPI()
            app.include_router(router)
            self.asgi_apps[str(api_path)] = app

        async def request() -> dict[str, Any]:
            import httpx
            from icebath_cli.plugin_host_http_auth import with_hosted_admission
            transport = httpx.ASGITransport(app=with_hosted_admission(app, params.get("authenticated")))
            async with httpx.AsyncClient(transport=transport, base_url="http://plugin-host") as client:
                response = await client.request(
                    str(params["method"]), str(params["path"]), params=str(params.get("query") or ""),
                    headers=[tuple(h) for h in params.get("headers") or []],
                    content=decode(params.get("body")) or b"")
            return {"status": response.status_code, "headers": list(response.headers.multi_items()),
                    "body": response.content}

        return encode(self._run(request()))

    def op_invoke(self, params: dict[str, Any]) -> Any:
        ref = int(params["ref"])
        fn = self.refs.get(ref)
        if fn is None:
            raise LookupError(f"plugin host callable {ref} was released")
        args, kwargs = self._decode_args(params, self.owners.get(ref, ""))
        return self._encode_result(self.owners.get(ref, ""), self._run(fn(*args, **kwargs)))

    def op_obj_invoke(self, params: dict[str, Any]) -> Any:
        ref = int(params["ref"])
        obj = self.refs.get(ref)
        if obj is None:
            raise LookupError(f"plugin host object {ref} was released")
        method = str(params["method"])
        if method.startswith("_"):
            raise AttributeError(method)
        args, kwargs = self._decode_args(params, self.owners.get(ref, ""))
        base = _provider_base(obj)
        if base is not None:  # the ABC's annotations say what Icebath passed (``home_path: Path``)
            args, kwargs = restore_arguments(declared_member(base, method), args, kwargs)
        return self._encode_result(self.owners.get(ref, ""), self._run(getattr(obj, method)(*args, **kwargs)))

    def op_obj_getattr(self, params: dict[str, Any]) -> Any:
        obj = self.refs.get(int(params["ref"]))
        name = str(params["name"])
        if obj is None:
            raise LookupError(f"plugin host object {params['ref']} was released")
        if name.startswith("_") or not hasattr(obj, name):
            return {"__missing__": name}
        return self._encode_result(self.owners.get(int(params["ref"]), ""), getattr(obj, name))

    def op_event(self, params: dict[str, Any]) -> Any:
        """One is_set, set, clear, wait or reason of a host event Icebath holds a ref to."""
        event = self.resolve_event({"__child_event__": params["ref"]})
        op = params.get("op")
        if op == "is_set":
            return event.is_set()
        if op == "set":
            return event.set()
        if op == "clear":
            return event.clear()
        if op == "wait":
            return event.wait(params.get("timeout"))
        if op == "reason":
            reason = getattr(event, "reason", None)
            return str(reason) if reason else ""
        raise ValueError(f"unknown plugin host event operation {op!r}")

    def op_release(self, params: dict[str, Any]) -> None:
        """Drop objects whose Icebath-side proxies were garbage-collected."""
        with self._lock:
            for ref in params.get("refs") or ():
                self.refs.pop(int(ref), None)
                self.owners.pop(int(ref), None)

    def op_config_schema(self, params: dict[str, Any]) -> Any:
        """A memory provider's ``config_schema.py`` ``CONFIG_SCHEMA`` (user code: it runs here)."""
        path = Path(str(params["path"]))
        spec = importlib.util.spec_from_file_location(f"_icebath_memory_config_schema.{path.parent.name}", path)
        if spec is None or spec.loader is None:
            raise ImportError(f"cannot load {path}")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return encode(getattr(module, "CONFIG_SCHEMA", None))

    def op_obj_setattr(self, params: dict[str, Any]) -> None:
        obj = self.refs.get(int(params["ref"]))
        name = str(params["name"])
        if obj is None or name.startswith("_"):
            raise AttributeError(name)
        setattr(obj, name, decode(params.get("value"), None))

    def op_unload(self, params: dict[str, Any]) -> dict[str, Any]:
        plugin_key = str(params["plugin_key"])
        errors = []
        for callback in reversed(self.unload_callbacks.pop(plugin_key, [])):
            try:
                self._run(callback())
            except Exception as exc:
                errors.append(f"{type(exc).__name__}: {exc}")
        with self._lock:
            for ref in [r for r, owner in self.owners.items() if owner == plugin_key]:
                self.refs.pop(ref, None)
                self.owners.pop(ref, None)
        module_name = self.modules.pop(plugin_key, "")
        if module_name:
            for name in [m for m in sys.modules if m == module_name or m.startswith(module_name + ".")]:
                sys.modules.pop(name, None)
        return {"errors": errors}

    def op_shutdown(self, _params: dict[str, Any]) -> None:
        for plugin_key in list(self.unload_callbacks):
            self.op_unload({"plugin_key": plugin_key})
        self.stopped.set()


_HANDLERS: dict[str, Callable[[HostRuntime, dict[str, Any]], Any]] = {
    "hello": HostRuntime.op_hello, "load": HostRuntime.op_load, "load_tools": HostRuntime.op_load_tools, "load_instance": HostRuntime.op_load_instance,
    "asgi": HostRuntime.op_asgi, "profile_call": HostRuntime.op_profile_call,
    "profile_client": HostRuntime.op_profile_client,
    "provider_entry_point_load": HostRuntime.op_provider_entry_point_load,
    "provider_entry_point_invoke": HostRuntime.op_provider_entry_point_invoke,
    "provider_module_call": HostRuntime.op_provider_module_call,
    "invoke": HostRuntime.op_invoke,
    "obj_invoke": HostRuntime.op_obj_invoke, "obj_getattr": HostRuntime.op_obj_getattr,
    "obj_setattr": HostRuntime.op_obj_setattr, "unload": HostRuntime.op_unload,
    "release": HostRuntime.op_release, "event": HostRuntime.op_event, "config_schema": HostRuntime.op_config_schema,
    "load_memory_entry_point": HostRuntime.op_load_memory_entry_point, "warm_import": HostRuntime.op_warm_import,
    "describe_memory_cli": HostRuntime.op_describe_memory_cli,
    "shutdown": HostRuntime.op_shutdown,
}


class _CapturingContext(RemotePluginContext):
    """``register(ctx)`` for a category plugin: the category's own registration is kept here; the rest
    forward to Icebath when it bound a context (memory providers), else are no-ops."""

    def __init__(self, runtime: HostRuntime, plugin_key: str, info: dict[str, Any], capture: str,
                 base: type, captured: list[Any], *, forward: bool):
        super().__init__(runtime, plugin_key, info)
        self._capture, self._base, self._captured, self._forward = capture, base, captured, forward

    def __getattr__(self, name: str) -> Any:
        if name == self._capture:
            def capture(obj: Any, *_args: Any, **_kwargs: Any) -> None:
                if isinstance(obj, self._base) and not self._captured:
                    self._captured.append(obj)
            return capture
        if not self._forward and (name.startswith("register_") or name in {"subscribe"}):
            return lambda *_args, **_kwargs: None
        return super().__getattr__(name)


class _CollectorContext(RemotePluginContext):
    """In-process collector of a bundled category plugin: the ``capture`` registration keeps its last value,
    every other attribute is answered by the Icebath collector bound to this plugin."""

    def __init__(self, runtime: HostRuntime, plugin_key: str, info: dict[str, Any], capture: str):
        super().__init__(runtime, plugin_key, info)
        self._capture, self._class, self.collected = capture, str(info.get("collector_class") or ""), None

    def __getattr__(self, name: str) -> Any:
        if name == self._capture:
            def capture(obj: Any) -> None:
                self.collected = obj
            return capture
        if name.startswith("_") or name not in self._methods:
            raise AttributeError(f"'{self._class}' object has no attribute {name!r}")
        return functools.partial(self._runtime.ctx_call, self._plugin_key, name)


class _ProviderCollector(RemotePluginContext):
    """``register(ctx)`` of a memory provider entry point: keeps the provider it registers, forwards the rest."""

    def __init__(self, runtime: HostRuntime, plugin_key: str, info: dict[str, Any]):
        super().__init__(runtime, plugin_key, info)
        self.provider = None

    def register_memory_provider(self, provider: Any) -> None:
        self.provider = provider

    def collect(self, register: Callable[..., Any], *, source: Optional[str] = None) -> None:
        """Icebath drops the fallback hooks of the register function's source file, then ``register`` runs."""
        if not source:
            module = sys.modules.get(getattr(register, "__module__", "") or "")
            source = getattr(module, "__file__", None)
        self._runtime.channel.call("collect_source", {"plugin": self._plugin_key, "source": source})
        register(self)


def _find_entry_point(group: str, name: str) -> Any:
    for entry_point in importlib.metadata.entry_points().select(group=group):
        if entry_point.name == name:
            return entry_point
    return None


def _instantiate_subclass(namespace: Any, base: type) -> Any:
    """First instantiable ``base`` subclass among *namespace*'s attributes."""
    for attr_name in dir(namespace):
        attr = getattr(namespace, attr_name, None)
        if isinstance(attr, type) and issubclass(attr, base) and attr is not base:
            try:
                return attr()
            except Exception:
                pass
    return None


def _entry_point_provider(runtime: HostRuntime, plugin_key: str, params: dict[str, Any], loaded: Any, base: type,
                          name: str) -> Any:
    if isinstance(loaded, base):
        return loaded
    if isinstance(loaded, type) and issubclass(loaded, base):
        try:
            return loaded()
        except Exception:
            pass
    if hasattr(loaded, "register"):
        collector = _ProviderCollector(runtime, plugin_key, params)
        collector.collect(loaded.register, source=getattr(loaded, "__file__", None))
        if collector.provider:
            return collector.provider
    if callable(loaded):
        try:
            provider = loaded()
            if isinstance(provider, base):
                return provider
        except TypeError:
            pass
        collector = _ProviderCollector(runtime, plugin_key, params)
        collector.collect(loaded)
        return collector.provider
    provider = _instantiate_subclass(loaded, base)
    if provider is None:
        logger.debug("Memory provider entry point '%s' loaded no provider", name)
    return provider


def _import_memory_cli(module_name: str, plugin_dir: Path) -> Any:
    """``<module_name>.cli`` from *plugin_dir*/cli.py under package shells, never executing the provider package."""
    cli_name = module_name + ".cli"
    cli_mod = sys.modules.get(cli_name)
    if cli_mod is None:
        from plugins.plugin_loader import register_synthetic_package

        register_synthetic_package(_USER_MEMORY_NAMESPACE, [])
        register_synthetic_package(module_name, [str(plugin_dir)])
        spec = importlib.util.spec_from_file_location(cli_name, str(plugin_dir / "cli.py"))
        if not spec or not spec.loader:
            return None
        cli_mod = importlib.util.module_from_spec(spec)
        sys.modules[cli_name] = cli_mod
        spec.loader.exec_module(cli_mod)
    return cli_mod


def _exit_code(code: Any) -> int:
    """The process status ``SystemExit(code)`` ends Python with, printing a non-integer code."""
    if code is None:
        return 0
    if isinstance(code, int):
        return code
    print(code, file=sys.stderr)
    return 1


def run_memory_cli_main(params_json: str, result_path: str) -> int:
    """Run a memory provider's argparse command on this process's stdio and write its outcome to *result_path*."""
    import argparse
    import json
    import traceback
    params = json.loads(params_json)
    name = str(params["name"])
    dispatched, rc = True, 0
    try:
        cli_mod = _import_memory_cli(str(params["module_name"]), Path(str(params["path"])))
        parser = argparse.ArgumentParser(prog=f"icebath {name}", description=params.get("description") or "",
                                         formatter_class=argparse.RawDescriptionHelpFormatter)
        cli_mod.register_cli(parser)
        if params.get("handler"):
            parser.set_defaults(func=getattr(cli_mod, f"{name}_command"))
        args = parser.parse_args([str(a) for a in params.get("argv") or []])
        func = getattr(args, "func", None)
        if func is None:
            dispatched = False
        else:
            result = func(args)
            rc = int(result) if isinstance(result, int) else 0
    except SystemExit as exc:
        rc = _exit_code(exc.code)
    except Exception:
        traceback.print_exc()
        rc = 1
    sys.stdout.flush()
    sys.stderr.flush()
    Path(result_path).write_text(json.dumps({"rc": rc, "dispatched": dispatched}), encoding="utf-8")
    return 0


def _import_base(ref: str) -> type:
    module, attr = ref.split(":")
    return getattr(importlib.import_module(module), attr)


def _provider_base(value: Any) -> Optional[type]:
    """The provider ABC (``HOST_OBJECT_BASES``) ``value`` implements. Only an already-imported base
    can match: a plugin object subclassing it imported it."""
    from icebath_cli.plugin_isolation import HOST_OBJECT_BASES
    for ref in HOST_OBJECT_BASES.values():
        module, attr = ref.split(":")
        base = getattr(sys.modules.get(module), attr, None)
        if isinstance(base, type) and isinstance(value, base):
            return base
    return None


def _is_provider_object(value: Any) -> bool:
    """A plugin-defined instance (provider/engine), as opposed to a function or bound method."""
    return not isinstance(value, (types.FunctionType, types.MethodType, types.BuiltinFunctionType,
                                  functools.partial, type)) and type(value).__module__ != "builtins"


async def _on_behalf_of(awaitable: Any, origin: Optional[int]) -> Any:
    bind_serving_request(origin)  # this task's context only; tasks it spawns inherit it
    return await awaitable


def _import_plugin(params: dict[str, Any]) -> Any:
    """Import a directory plugin under the module name Icebath assigned, or resolve an entry point."""
    if params.get("entrypoint"):
        for ep in importlib.metadata.entry_points().select(group=_ENTRY_POINTS_GROUP):
            if ep.name == params.get("name"):
                return ep.load()
        raise ImportError(f"Entry point '{params.get('name')}' not found in group '{_ENTRY_POINTS_GROUP}'")
    plugin_dir = Path(str(params["path"]))
    init_file = plugin_dir / "__init__.py"
    if not init_file.exists():
        raise FileNotFoundError(f"No __init__.py in {plugin_dir}")
    module_name = str(params["module_name"])
    parts = module_name.split(".")[:-1]
    for i in range(1, len(parts) + 1):  # synthetic parent packages (icebath_plugins, _icebath_user_memory)
        parent = ".".join(parts[:i])
        if parent not in sys.modules:
            ns_pkg = types.ModuleType(parent)
            ns_pkg.__path__ = []  # type: ignore[attr-defined]
            ns_pkg.__package__ = parent
            sys.modules[parent] = ns_pkg
    spec = importlib.util.spec_from_file_location(module_name, init_file,
                                                  submodule_search_locations=[str(plugin_dir)])
    if spec is None or spec.loader is None:
        raise ImportError(f"Cannot create module spec for {init_file}")
    module = importlib.util.module_from_spec(spec)
    module.__package__ = module_name
    module.__path__ = [str(plugin_dir)]  # type: ignore[attr-defined]
    sys.modules[module_name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        for name in [m for m in sys.modules if m == module_name or m.startswith(module_name + ".")]:
            sys.modules.pop(name, None)
        raise
    return module


def _load_provider_package(module_name: str, plugin_dir: Path, parents: list, plugins_root: Path,
                           synthetic_namespace: Optional[str]) -> Any:
    """The provider package the memory loader names, through plugins.plugin_loader.load_plugin_module."""
    from plugins import plugin_loader

    return plugin_loader.load_plugin_module(module_name, plugin_dir, parents=tuple(parents), logger=logger,
                                            synthetic_namespace=synthetic_namespace, plugins_root=plugins_root)


def _take_protocol_streams():
    """Duplicate stdin/stdout for the channel, then point fd 0 at /dev/null and fd 1 at stderr."""
    reader = os.fdopen(os.dup(0), "rb")  # windows-footgun: ok — binary protocol stream
    writer = os.fdopen(os.dup(1), "wb")  # windows-footgun: ok — binary protocol stream
    devnull = os.open(os.devnull, os.O_RDONLY)
    os.dup2(devnull, 0)
    os.close(devnull)
    os.dup2(2, 1)
    sys.stdin = open(os.devnull, encoding="utf-8-sig")
    sys.stdout = sys.stderr
    return reader, writer


def capture_profiles(path: str, module_name: str) -> list[Any]:
    """Import a model-provider plugin and return the profiles it passes to ``register_provider``."""
    import providers
    providers.list_providers()  # bundled + known profiles register before the capture window
    with _capturing() as captured:
        _import_plugin({"path": path, "module_name": module_name, "name": Path(path).name})
    return captured


_PROFILE_CAPTURES: list[list[Any]] = []
_PROFILE_CAPTURE_LOCK = threading.RLock()
_ENTRY_POINT_PROFILE_KEY = "entrypoint:"
_PROFILE_CLIENT_OWNER = "profile-client:"


class _capturing:
    """Collect every ``register_provider`` call made inside the block, even through a name bound earlier."""

    def __enter__(self) -> list[Any]:
        import providers
        providers.list_providers()  # bundled + known profiles register before the capture window
        _PROFILE_CAPTURE_LOCK.acquire()
        self.captured: list[Any] = []
        _PROFILE_CAPTURES.append(self.captured)
        self.original = providers.register_provider
        if self.original is not _capturing_register:
            _ORIGINAL_REGISTER[:] = [self.original]
            providers.register_provider = _capturing_register  # type: ignore[assignment]
        return self.captured

    def __exit__(self, *exc: Any) -> None:
        import providers
        _PROFILE_CAPTURES.pop()
        providers.register_provider = self.original  # type: ignore[assignment]
        _PROFILE_CAPTURE_LOCK.release()


_ORIGINAL_REGISTER: list[Callable[[Any], None]] = []


def _capturing_register(profile: Any) -> None:
    """Record into the innermost capture, or register for real when no capture is open."""
    if _PROFILE_CAPTURES:
        _PROFILE_CAPTURES[-1].append(profile)
        return
    _ORIGINAL_REGISTER[-1](profile)


def _requires_arguments(fn: Any) -> bool:
    """True when ``fn`` cannot be called with zero arguments, unintrospectable callables counting as zero-arg."""
    try:
        sig = inspect.signature(fn)
    except (TypeError, ValueError):
        return False
    for param in sig.parameters.values():
        if param.kind in (inspect.Parameter.POSITIONAL_ONLY, inspect.Parameter.POSITIONAL_OR_KEYWORD,
                          inspect.Parameter.KEYWORD_ONLY) and param.default is inspect.Parameter.empty:
            return True
    return False


def _is_live_client_part(value: Any) -> bool:
    """Whether a value reached from a live SDK client stays in the host as an object instead of data."""
    import dataclasses
    if callable(value) or isinstance(value, type) or dataclasses.is_dataclass(value):
        return False
    return not callable(getattr(value, "model_dump", None))


def _host_unsupported_reason(profile: Any) -> str | None:
    """Why *profile* cannot run behind the plugin host (it then loads in-process), or None."""
    from providers.base import ProviderProfile
    if type(profile).create_client is not ProviderProfile.create_client:
        return "it overrides create_client(), which returns a live SDK client"
    return None


def _extraction_entry(profile: Any) -> dict[str, Any]:
    reason = _host_unsupported_reason(profile)
    if reason is not None:
        return {**describe_profile(profile), "in_process": reason}
    return describe_profile(profile)


def describe_profile(profile: Any) -> dict[str, Any]:
    """Profile data (instance fields) plus the methods and callable fields that must run here."""
    from providers.base import ProviderProfile
    fields: dict[str, Any] = {}
    calls: dict[str, Any] = {}
    for name, value in vars(profile).items():
        if name.startswith("_"):
            continue
        if callable(value):
            calls[name] = {"async": is_async_callable(value), "sig": describe_signature(value), "field": True}
        else:
            fields[name] = encode(value)
    for name in dir(type(profile)):
        if name.startswith("_") or name in fields or name in calls:
            continue
        attr = getattr(type(profile), name)
        if attr is getattr(ProviderProfile, name, object()):
            continue
        if isinstance(inspect.getattr_static(profile, name), property) or not callable(attr):
            fields[name] = encode(getattr(profile, name))
            continue
        calls[name] = {"async": is_async_callable(attr), "sig": describe_signature(getattr(profile, name)),
                       "field": False}
    return {"name": profile.name, "type": type(profile).__name__, "fields": fields, "calls": calls}


def extract_profiles_main(path: str, module_name: str) -> int:
    """``--extract-profiles``: one-shot, credential-free; prints the profiles as JSON on stdout."""
    import json
    reader, writer = _take_protocol_streams()
    del reader
    try:
        payload = {"profiles": [_extraction_entry(p) for p in capture_profiles(path, module_name)]}
    except Exception as exc:
        payload = {"error": f"{type(exc).__name__}: {exc}"}
    writer.write(json.dumps(payload).encode("utf-8"))
    writer.flush()
    return 0


def main() -> int:
    reader, writer = _take_protocol_streams()
    logging.basicConfig(stream=sys.stderr, level=os.environ.get("ICEBATH_PLUGIN_HOST_LOG_LEVEL", "WARNING"),
                        format="%(levelname)s %(name)s: %(message)s")
    runtime = HostRuntime(reader, writer)
    runtime.channel.start()
    runtime.stopped.wait()
    runtime.channel.close("host shutdown")
    return 0


def _deny_network(*_args: Any, **_kwargs: Any) -> None:
    raise RuntimeError("network access is disabled while Plugin Doctor runs")


def _apply_network_denial() -> None:
    """Plugin Doctor children block outbound socket connects before any plugin code runs."""
    if os.environ.pop("ICEBATH_PLUGIN_HOST_DENY_NETWORK", "") != "1":
        return
    import socket
    socket.create_connection = _deny_network
    socket.socket.connect = _deny_network
    socket.socket.connect_ex = _deny_network


if __name__ == "__main__":
    _apply_network_denial()
    if sys.argv[1:2] == ["--extract-profiles"]:
        code = extract_profiles_main(sys.argv[2], sys.argv[3])
    elif sys.argv[1:2] == ["--run-memory-cli"]:
        code = run_memory_cli_main(sys.argv[2], sys.argv[3])
    else:
        code = main()
    sys.stderr.flush()
    # A plugin thread stuck in a handler must not keep the host alive once Icebath has gone.
    os._exit(code)
