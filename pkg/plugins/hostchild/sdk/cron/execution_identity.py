"""The cron occurrence ``ctx.current_cron_execution()`` reports, built from the identity Icebath binds
for the fire the calling code runs inside."""
from __future__ import annotations

from dataclasses import dataclass
from typing import Optional


@dataclass(frozen=True)
class CronExecution:
    job_id: str
    job_name: str
    execution_id: str
    source: str
    scheduled_instant: Optional[str]
    started_at: Optional[str]
    profile: str
