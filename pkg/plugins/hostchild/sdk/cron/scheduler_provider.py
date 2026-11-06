"""CronScheduler provider interface (the trigger). Providers decide only *when* a job fires; the
claim, the audit attempt and the run stay in Icebath and are reached over the plugin host channel."""
from __future__ import annotations

import inspect
import threading
from abc import ABC, abstractmethod
from typing import Any

from icebath_cli.plugin_sdk import parent_call

DEFAULT_MISFIRE_GRACE_MINUTES = 10


class CronScheduler(ABC):
    """Decides WHEN a due cron job fires. Only ``name`` + ``start`` are required."""

    @property
    @abstractmethod
    def name(self) -> str:
        """Short identifier, e.g. 'builtin', 'chronos'."""

    def is_available(self) -> bool:
        """Whether this provider can run here. MUST NOT make network calls; False -> built-in."""
        return True

    @abstractmethod
    def start(
        self, stop_event: threading.Event, *, adapters: Any = None, loop: Any = None,
        interval: int = 60,
    ) -> None:
        """Begin firing due jobs; an external provider may return immediately but must honor stop_event."""

    def stop(self) -> None:
        """Optional eager teardown; stop_event is the primary signal."""
        return

    def on_jobs_changed(self) -> None:
        """After a successful store mutation; external providers reconcile. Built-in: no-op."""
        return

    def register_job(self, job: dict[str, Any]) -> None:
        """Register the external trigger for a newly persisted job. Built-in: no-op."""
        return

    def recover_interrupted(self) -> int:
        """Run profile-local attempt recovery for every provider lifecycle."""
        return parent_call("cron.recover_interrupted")

    @property
    def supports_force_fire(self) -> bool:
        """Whether ``fire_due`` accepts ``force`` (signature-detected for older providers)."""
        return provider_supports_force_fire(self)

    def fire_due(
        self, job_id: str, *, adapters: Any = None, loop: Any = None, force: bool = False,
        manual: bool = False,
    ) -> bool:
        """Run one job NOW: the store claim then the shared run. True if THIS caller claimed and processed it."""
        claimed_job = self.claim_fire(job_id, force=force, manual=manual)
        if claimed_job is None:
            return False
        return self.fire_claimed(claimed_job, adapters=adapters, loop=loop)

    def claim_fire(self, job_id: str, *, force: bool = False, manual: bool = False) -> dict | None:
        """Durably claim one fire and create its audit attempt; None when the claim was not acquired."""
        return parent_call("cron.claim_fire", job_id, source=self.name, force=force, manual=manual)

    def fire_claimed(
        self, claimed_job: dict, *, adapters: Any = None, loop: Any = None,
        cancel_event: Any = None,
    ) -> bool:
        """Run an exact ``claim_fire`` snapshot; ``cancel_event`` stops it cooperatively."""
        parent_call("cron.fire_claimed", claimed_job, adapters=adapters, loop=loop, cancel_event=cancel_event)
        return True

    def reconcile(self) -> None:
        """Converge the external registry toward the job store. Built-in: no-op."""
        return


def provider_supports_force_fire(provider: Any) -> bool:
    """Return whether a provider can safely receive ``fire_due(force=...)`` (signature-detected)."""
    return provider_fire_due_accepts(provider, "force")


def provider_fire_due_accepts(provider: Any, name: str) -> bool:
    """Whether ``provider.fire_due`` takes keyword ``name`` (third-party providers may predate it)."""
    try:
        parameters = inspect.signature(provider.fire_due).parameters.values()
    except (TypeError, ValueError):
        return False
    return any(
        p.kind is inspect.Parameter.VAR_KEYWORD
        or (
            p.name == name
            and p.kind in (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)
        )
        for p in parameters
    )


def provider_supports_split_fire(provider: Any) -> bool:
    """Whether a provider implements the two-phase fire contract instead of overriding only ``fire_due``."""
    cls = type(provider)

    def overrides(name: str) -> bool:
        impl = getattr(cls, name, None)
        return impl is not None and impl is not getattr(CronScheduler, name)

    if overrides("claim_fire") or overrides("fire_claimed"):
        return True
    return not overrides("fire_due")
