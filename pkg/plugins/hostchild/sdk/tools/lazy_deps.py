"""Shims to suppress old updater work until relaunch. New code must not use these."""

from typing import NoReturn


def ensure(feature: str, *, prompt: bool = True) -> NoReturn:
    # Shim to suppress old updater work until relaunch. Do not claim readiness.
    # Preserve the dependency-unavailable failure without claiming a completed install.
    raise ImportError("Dependencies are unknown to this old updater. Please relaunch Icebath.")


def install_specs(specs: list[str] | tuple[str, ...], *, timeout: int = 300,
                  constraints: list | None = None, dry_run: bool = False) -> NoReturn:
    # Plugins still call this retired API during normal agent construction.
    # A plugin host child is never an updater call stack, so it never hands control to the updater.
    # Historical updaters also passed constraints/dry_run; accepted and ignored.
    raise ImportError(
        "tools.lazy_deps.install_specs is retired; runtime dependency "
        "installation is unavailable."
    )
