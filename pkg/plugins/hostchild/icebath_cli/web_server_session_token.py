"""The dashboard session token as the plugin host child resolves it."""

import os
import secrets
from pathlib import Path


def resolve_session_token() -> str:
    """The minted token, a file: value read from its path, or a fresh one per process."""
    env_token = os.environ.get("ICEBATH_DASHBOARD_SESSION_TOKEN")
    if not env_token:
        return secrets.token_urlsafe(32)
    if env_token.startswith("file:"):
        token_file = env_token[len("file:") :]
        try:
            token = Path(token_file).read_text(encoding="utf-8-sig").strip()
        except (OSError, UnicodeError) as exc:
            raise SystemExit(
                f"ICEBATH_DASHBOARD_SESSION_TOKEN names an unreadable token file ({token_file}): {exc}"
            ) from exc
        if not token:
            raise SystemExit(
                f"ICEBATH_DASHBOARD_SESSION_TOKEN names an empty token file ({token_file})"
            )
        return token
    return env_token
