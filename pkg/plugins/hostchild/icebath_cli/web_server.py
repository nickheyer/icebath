"""The dashboard route guard as hosted plugin APIs reach it inside the plugin host."""

import hmac

from fastapi import HTTPException, Request

from icebath_cli.web_server_session_token import resolve_session_token

_SESSION_TOKEN = resolve_session_token()
_SESSION_HEADER_NAME = "X-Icebath-Session-Token"


def _has_valid_session_token(request: Request) -> bool:
    """True when the session header or the legacy Bearer header carries this process token."""
    session_header = request.headers.get(_SESSION_HEADER_NAME, "")
    if session_header and hmac.compare_digest(session_header.encode(), _SESSION_TOKEN.encode()):
        return True
    auth = request.headers.get("authorization", "")
    return hmac.compare_digest(auth.encode(), f"Bearer {_SESSION_TOKEN}".encode())


def _require_token(request: Request) -> None:
    """Raises 401 unless an admitted principal, a gated session or the loopback token is present."""
    if (getattr(request.state, "native_http_principal", None)
            or getattr(request.state, "hosted_http_authenticated", None) is True):
        return
    if getattr(request.app.state, "auth_required", False):
        ok = getattr(request.state, "session", None) is not None
    else:
        ok = _has_valid_session_token(request)
    if not ok:
        raise HTTPException(status_code=401, detail="Unauthorized")
