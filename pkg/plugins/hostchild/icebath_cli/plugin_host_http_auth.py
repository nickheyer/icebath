"""Carry route-local admission over the private plugin-host channel, without credentials."""
from fastapi import HTTPException


def admitted_for_sensitive_route(request) -> bool:
    """Preserve the normal guard's decision without changing unguarded plugin routes."""
    try:
        _require_token(request)
    except HTTPException as exc:
        if exc.status_code != 401:
            raise
        return False
    return True


def _require_token(request) -> None:
    """The dashboard guard as it decides inside the host: only an admitted principal passes."""
    if (getattr(request.state, "native_http_principal", None)
            or getattr(request.state, "hosted_http_authenticated", None) is True):
        return
    raise HTTPException(status_code=401, detail="Unauthorized")


def with_hosted_admission(app, authenticated):
    """Admission belongs to this IPC request, never to the cached plugin app."""
    async def scoped(scope, receive, send):
        state = dict(scope.get("state") or {})
        state["hosted_http_authenticated"] = authenticated is True
        await app({**scope, "state": state}, receive, send)
    return scoped
