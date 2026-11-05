"""Subprocess environments for helper children a plugin starts, built by Icebath's env factory."""

from __future__ import annotations

import os
from typing import Mapping

from icebath_cli.plugin_sdk import parent_call


def build_subprocess_env(
    base: Mapping[str, str] | None = None, *, inherit_profile_home: bool = True,
    scrub_secrets: bool = True, extra: Mapping[str, str] | None = None,
    strip_launch_profile: bool = False) -> dict[str, str]:
    """Child env from *base* (this host's environment by default) with Icebath's blocklist and scrubbing."""
    return dict(parent_call("build_subprocess_env", base=dict(os.environ if base is None else base),
                            inherit_profile_home=inherit_profile_home, scrub_secrets=scrub_secrets,
                            extra=dict(extra) if extra else None, strip_launch_profile=strip_launch_profile))
