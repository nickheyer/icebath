"""``$ICEBATH_HOME/cache/<kind>/`` materialisation helpers for the image/video
generation provider ABCs.

Several backends return *ephemeral* delivery URLs that expire before a downstream
consumer (Telegram ``send_photo``, browser fetch) can resolve them, so providers
materialise the bytes locally at tool-completion time. Filenames are
``<prefix>_<YYYYMMDD_HHMMSS>_<uuid8>.<ext>``.
"""

from __future__ import annotations

import base64
import datetime
import uuid
from pathlib import Path
from typing import Dict, Optional, Tuple
from urllib.parse import urljoin

# Generated deliverables live in ``cache/generated/<media>/``, OUTSIDE the transient
# inbound caches the gateway housekeeping sweeps (#126445): for base64-returning
# providers the cached file is the ONLY copy of the output.
GENERATED_SUBDIR = "generated"
# Age (hours) past which the gateway sweeps transient media caches; remote sync of the
# unswept ``cache/generated`` tree uses the same window so it stays bounded.
MEDIA_CACHE_MAX_AGE_HOURS = 24

_REDIRECT_STATUS_CODES = {301, 302, 303, 307, 308}
_MAX_SAVE_URL_REDIRECTS = 5


def cache_dir(kind: str) -> Path:
    """Return ``$ICEBATH_HOME/cache/<kind>/``, creating parents as needed."""
    from icebath_constants import get_icebath_home
    path = get_icebath_home() / "cache" / kind
    path.mkdir(parents=True, exist_ok=True)
    return path


def cache_path(kind: str, prefix: str, extension: str) -> Path:
    ts = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
    short = uuid.uuid4().hex[:8]
    return cache_dir(kind) / f"{prefix}_{ts}_{short}.{extension}"


def save_bytes(kind: str, raw: bytes, *, prefix: str, extension: str) -> Path:
    """Write raw bytes to the cache and return the absolute path."""
    path = cache_path(kind, prefix, extension)
    path.write_bytes(raw)
    return path


def save_b64(kind: str, b64_data: str, *, prefix: str, extension: str) -> Path:
    """Decode base64 data into the cache and return the absolute path."""
    return save_bytes(kind, base64.b64decode(b64_data), prefix=prefix, extension=extension)


def save_url(
    kind: str, url: str, *, prefix: str, timeout: float, max_bytes: int, chunk_size: int,
    content_types: dict[str, str], url_extensions: tuple[str, ...], default_extension: str,
    label: str, empty_error: str, headers: Optional[dict[str, str]] = None,
    require_known_content_type: bool = False, trusted_origin: bool = False,
) -> Path:
    """Stream-download *url* into the cache with a size cap; Icebath performs the SSRF-guarded fetch."""
    from icebath_cli.plugin_sdk import parent_call
    return Path(parent_call(
        "provider_media.save_url", kind, url, prefix=prefix, timeout=timeout, max_bytes=max_bytes,
        chunk_size=chunk_size, content_types=dict(content_types), url_extensions=list(url_extensions),
        default_extension=default_extension, label=label, empty_error=empty_error,
        headers=dict(headers) if headers else None, require_known_content_type=require_known_content_type,
        trusted_origin=trusted_origin))
