"""Credentialed HTTP requests of plugin code, sent by Icebath under its redirect and TLS policy."""

from __future__ import annotations

import email.message
import io
import urllib.request
from typing import Any, Optional

from icebath_cli.plugin_sdk import parent_call


class CredentialedResponse(io.BytesIO):
    """The response Icebath fetched: ``read()``, ``status``, ``getcode()``, ``headers``, ``url``."""

    def __init__(self, reply: dict[str, Any]):
        super().__init__(bytes(reply.get("body") or b""))
        self.status = int(reply.get("status") or 0)
        self.url = str(reply.get("url") or "")
        self.headers = email.message.Message()
        for name, value in reply.get("headers") or []:
            self.headers[str(name)] = str(value)

    def getcode(self) -> int:
        return self.status

    def geturl(self) -> str:
        return self.url

    def info(self) -> email.message.Message:
        return self.headers


def open_credentialed_url(req: urllib.request.Request | str, timeout: Optional[float] = None,
                          **kwargs: Any) -> CredentialedResponse:
    """Open *req* through Icebath; credentials never follow a redirect off the original origin."""
    if isinstance(req, str):
        req = urllib.request.Request(req)
    return CredentialedResponse(parent_call(
        "open_credentialed_url", req.full_url, method=req.get_method(), headers=list(req.header_items()),
        data=req.data, timeout=timeout, **kwargs))
