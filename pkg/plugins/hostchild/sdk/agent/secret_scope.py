"""Profile-scoped credential resolution for multi-profile gateway multiplexing.

The multiplexing gateway serves many profiles from one process; each profile's
``.env`` keys **cannot** be unioned into ``os.environ`` (profile A's keys would
leak into profile B's turns and subprocesses). This module is a fail-closed,
context-local secret scope: ``set_secret_scope(mapping)`` installs the active
profile's secrets for the current task (a contextvar, so it propagates into the
agent's worker thread via ``copy_context()``); ``get_secret(name)`` reads from
it and, when multiplexing is active with no scope set, RAISES rather than
falling back to ``os.environ``. Design: ``website/docs/developer-guide/multiplexing-gateway.md``.
"""
from __future__ import annotations

import codecs
import os
import re
import threading
from collections import OrderedDict
from contextvars import ContextVar, Token
from pathlib import Path
from typing import Callable, Dict, Mapping, NamedTuple, Optional, Tuple


# Process-global (describes the deployment mode, not a per-task value): set once
# at gateway startup when gateway.multiplex_profiles is true.
_MULTIPLEX_ACTIVE: bool = False
# Launch home pinned by set_multiplex_active(True) itself (None: no auto-pin outstanding).
_AUTO_PINNED_HOME = None

# Context-local counterpart: a task serving a profile OTHER than the process's own, inside a
# process that is not a multiplexer as a whole — the desktop backend's cron ticker firing a
# sibling profile's job. Every isolation keyed on ``is_multiplex_active()`` (the routed-dotenv
# guard, ``get_secret``'s fail-closed miss, subprocess scrubbing, passthrough) applies inside
# it while the process's own turns keep single-profile semantics. A contextvar, so it reaches
# the pool worker together with the home override via ``copy_context()``.
_MULTIPLEX_CONTEXT: ContextVar[bool] = ContextVar("_MULTIPLEX_CONTEXT", default=False)


def set_multiplex_active(active: bool) -> None:
    """Mark whether the process is a profile multiplexer (get_secret fails closed).

    Activation also pins the launch home for routed-profile decisions
    (``icebath_constants.pin_process_icebath_home``) unless an embedding host already pinned one:
    from here on "is this task routed" compares the override against the home the process was
    launched with, not against whatever a host later mirrors into ``os.environ["ICEBATH_HOME"]``.
    Deactivation releases only the pin activation itself created — a transient toggle
    (``gateway_migrate._multiplex_read_mode``, a cron worker restoring the caller's mode) must not
    drop the host's explicit pin (#119242)."""
    global _MULTIPLEX_ACTIVE, _AUTO_PINNED_HOME
    from icebath_constants import (
        get_routing_process_icebath_home,
        pin_process_icebath_home,
        process_icebath_home_is_pinned,
    )
    _MULTIPLEX_ACTIVE = bool(active)
    if _MULTIPLEX_ACTIVE:
        if not process_icebath_home_is_pinned():
            _AUTO_PINNED_HOME = get_routing_process_icebath_home()
            pin_process_icebath_home(_AUTO_PINNED_HOME)
    elif _AUTO_PINNED_HOME is not None:
        if get_routing_process_icebath_home() == _AUTO_PINNED_HOME:
            pin_process_icebath_home(None)
        _AUTO_PINNED_HOME = None


def set_multiplex_context(active: bool) -> Token:
    """Run the current task under multiplex semantics regardless of the process flag.
    Returns a reset token; pair with :func:`reset_multiplex_context` in a ``finally``."""
    return _MULTIPLEX_CONTEXT.set(bool(active))


def reset_multiplex_context(token: Token) -> None:
    _MULTIPLEX_CONTEXT.reset(token)


def is_multiplex_active() -> bool:
    """True in a multiplexing process, or for a task running under multiplex semantics."""
    return _MULTIPLEX_ACTIVE or _MULTIPLEX_CONTEXT.get()


class _BoundScope(NamedTuple):
    """An installed secret scope plus the home it was built for, when the binder
    declared one — the provenance ``serves_routed_profile`` needs when the binding
    deliberately skips the ICEBATH_HOME override (kanban spawn-env builds, MCP
    owner scopes)."""

    mapping: Mapping[str, str]
    profile_home: Optional[str]


def serves_routed_profile() -> bool:
    """True when the current task runs for a profile other than the process's own: always under
    multiplexing, else when a ICEBATH_HOME override names another home (dashboard/desktop backend,
    per-profile cron ticker) or a secret scope stamped with a foreign home is bound. The MCP
    registry scope and the check_fn cache key both follow this predicate so a served profile's
    view never aliases the launch profile's (#111151). A host that mirrors the turn's profile into
    ``ICEBATH_HOME`` pins its own home with ``icebath_constants.pin_process_icebath_home`` so the
    mirror cannot flip this predicate."""
    if is_multiplex_active():
        return True
    from icebath_constants import get_icebath_home_override, get_routing_process_icebath_home, icebath_home_key
    own = icebath_home_key(get_routing_process_icebath_home())
    bound = _SECRET_SCOPE.get()
    if bound is not None and bound.profile_home and icebath_home_key(bound.profile_home) != own:
        return True
    override = get_icebath_home_override()
    return override is not None and icebath_home_key(override) != own


_SECRET_SCOPE: ContextVar[Optional[_BoundScope]] = ContextVar("_SECRET_SCOPE", default=None)
# A launch-profile body that entered before this process started multiplexing (no scope bound,
# single-profile semantics): the launch profile's own mapping to read once fail-closed is armed
# mid-body. Set only by ``tui_gateway.launch_profile_policy.launch_body``; never ``os.environ``.
_LAUNCH_BODY_SCOPE: ContextVar[Optional[Callable[[], Mapping[str, str]]]] = ContextVar(
    "_LAUNCH_BODY_SCOPE", default=None)


def set_launch_body_scope(resolver: Callable[[], Mapping[str, str]]) -> Token:
    return _LAUNCH_BODY_SCOPE.set(resolver)


def reset_launch_body_scope(token: Token) -> None:
    _LAUNCH_BODY_SCOPE.reset(token)


class UnscopedSecretError(RuntimeError):
    """A secret was read in multiplex mode with no scope installed.

    The fix is to wrap the call path in ``set_secret_scope(...)`` (the per-turn
    / per-adapter profile scope), not to widen the global allowlist.

    ``str(exc)`` is the ONE sentence an end user can act on; the developer diagnosis
    (which secret, which doc) rides ``__notes__`` so tracebacks and logs keep it.
    """

    def __init__(self, secret_name: str = "", developer_detail: str = ""):
        # Older callers passed the whole developer sentence positionally
        # (``UnscopedSecretError("get_secret('X') called with no scope ...")``); a secret
        # name never contains whitespace, so treat such a string as the detail.
        if secret_name and not developer_detail and any(ch.isspace() for ch in secret_name):
            secret_name, developer_detail = "", secret_name
        what = f"this profile's {secret_name}" if secret_name else "this profile's API key"
        super().__init__(
            f"Icebath could not read {what} (an internal profile-scoping bug on the multiplexed "
            "gateway, not your configuration). Run `icebath gateway restart`; if it keeps happening, "
            "report it with `icebath debug share`."
        )
        self.secret_name = secret_name
        self.developer_detail = developer_detail
        if developer_detail:
            self.add_note(developer_detail)


def set_secret_scope(secrets: Optional[Mapping[str, str]], *, profile_home: Optional[str] = None) -> Token:
    """Install the active profile's secret mapping; ``None`` clears. Returns a reset token.

    ``profile_home`` stamps the home the mapping was built for so
    ``serves_routed_profile`` detects a foreign-home scope even when the binder
    deliberately skips the ICEBATH_HOME override."""
    if secrets is None:
        return _SECRET_SCOPE.set(None)
    return _SECRET_SCOPE.set(_BoundScope(secrets, str(profile_home) if profile_home else None))


def reset_secret_scope(token: Token) -> None:
    _SECRET_SCOPE.reset(token)


def current_secret_scope() -> Optional[Mapping[str, str]]:
    """The active secret mapping, or None when no scope is installed."""
    bound = _SECRET_SCOPE.get()
    return bound.mapping if bound is not None else None


def current_secret_scope_home() -> Optional[str]:
    """The home the active scope was stamped with, or None when unstamped/unbound."""
    bound = _SECRET_SCOPE.get()
    return bound.profile_home if bound is not None else None


# Genuinely-global env vars: process/deployment settings, NOT profile secrets.
# They keep reading os.environ even in multiplex mode (routing them through the
# fail-closed path would wrongly crash). Keep this tight — when in doubt a
# value is a profile secret. Membership is exact name OR prefix.
_GLOBAL_ENV_EXACT = frozenset({
    # Icebath runtime / deployment
    "ICEBATH_HOME", "ICEBATH_PROFILE", "ICEBATH_GATEWAY_LOCK_DIR",
    "ICEBATH_MAX_ITERATIONS", "ICEBATH_API_TIMEOUT",
    "ICEBATH_REDACT_SECRETS", "ICEBATH_NOUS_TIMEOUT_SECONDS",
    "_ICEBATH_GATEWAY",
    # OS / interpreter
    "PATH", "HOME", "USER", "LANG", "LC_ALL", "TZ", "PWD", "SHELL", "TMPDIR",
    "VIRTUAL_ENV", "PYTHONPATH", "SSL_CERT_FILE",
    # Kanban paths (per-board, not per-profile-secret)
    "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_WORKSPACES_ROOT", "ICEBATH_KANBAN_BOARD",
    # API-server LISTENER settings — deployment config (compose/systemd env),
    # which the scoped runner reload must keep seeing or containers silently
    # lose the api_server platform. API_SERVER_KEY is a credential: NOT here.
    # See #64674, #69379.
    "API_SERVER_ENABLED", "API_SERVER_HOST", "API_SERVER_PORT",
    "API_SERVER_CORS_ORIGINS",
    # Relay-connector ROUTING stamps injected by managed deploys. Every reader
    # (gateway.config, relay_url()/registration/self-provision) must resolve
    # the SAME value or the adapter registers while the platform is absent
    # from config. GATEWAY_RELAY_SECRET/_ID/_DELIVERY_KEY and IDP_* are auth
    # material and deliberately stay profile-scoped.
    "GATEWAY_RELAY_URL", "GATEWAY_RELAY_ENDPOINT",
    "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS",
    "GATEWAY_RELAY_PLATFORMS", "GATEWAY_RELAY_BOT_IDS",
    "GATEWAY_RELAY_ROUTE_KEYS", "GATEWAY_RELAY_INSTANCE_ID",
    "GATEWAY_RELAY_WAKE_URL", "GATEWAY_RELAY_DISPLAY_NAME",
})
_GLOBAL_ENV_PREFIXES = (
    "ICEBATH_KANBAN_",
    "ICEBATH_TELEGRAM_",   # tuning knobs (batch delays, fallback toggles) — NOT the token
    "TERMINAL_",          # terminal/sandbox backend settings
)


def _is_global_env(name: str) -> bool:
    """True for genuinely process-global (non-profile-secret) env vars."""
    return name in _GLOBAL_ENV_EXACT or name.startswith(_GLOBAL_ENV_PREFIXES)


def _environ_or(name: str, default: Optional[str]) -> Optional[str]:
    val = os.environ.get(name)
    return val if val is not None else default


def get_secret(name: str, default: Optional[str] = None) -> Optional[str]:
    """Resolve a credential by env-var name, honoring the active profile scope.

    Global vars always read ``os.environ``. With a scope installed, a miss returns
    ``default`` under multiplexing (never another profile's ``os.environ`` value)
    but falls through to ``os.environ`` otherwise — single-profile deployments
    inject credentials via the process env (systemd, ``op run``), so the scope
    must stay a ``.env`` overlay, not a blindfold (otherwise cron 401s). With no
    scope: multiplex INACTIVE reads ``os.environ``; ACTIVE raises (fail closed).
    """
    if _is_global_env(name):
        return _environ_or(name, default)
    bound = _SECRET_SCOPE.get()
    if bound is not None:
        val = bound.mapping.get(name)
        if val is not None:
            return val
        return default if serves_routed_profile() else _environ_or(name, default)
    if is_multiplex_active():
        launch_scope = _LAUNCH_BODY_SCOPE.get()
        if launch_scope is not None:
            return launch_scope().get(name, default)
        raise UnscopedSecretError(
            name,
            f"get_secret({name!r}) called with no profile secret scope active "
            f"while multiplexing is on. This credential read must run inside a "
            f"set_secret_scope(...) block (the per-turn / per-adapter profile "
            f"scope). Reading os.environ here would risk leaking another "
            f"profile's value. See website/docs/developer-guide/multiplexing-gateway.md "
            f"(Workstream A).",
        )
    return _environ_or(name, default)


def secret_source_label(name: str) -> str:
    """``" (from Bitwarden)"``-style suffix naming the external secret source (a secret-source plugin)
    that supplied env var *name*; ``""`` for ``.env`` / shell values. Public for plugins' status lines."""
    from icebath_cli.env_loader import format_secret_source_suffix

    return format_secret_source_suffix(name)


def get_secret_str(name: str, default: str = "") -> str:
    """``get_secret`` for callers that want a ``str``: ``default`` only when the secret is genuinely
    unset. Still raises ``UnscopedSecretError`` — swallowing it hides a spawn-site bug."""
    val = get_secret(name, default)
    return default if val is None else val


