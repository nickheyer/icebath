"""Base URL and proxy URL helpers of the Icebath utils module that the plugin SDK ships."""

from urllib.parse import urlparse


def normalize_proxy_url(proxy_url: str | None) -> str | None:
    """Normalize proxy URLs for httpx/aiohttp: WSL/Clash export ``socks://``, httpx needs ``socks5://``."""
    candidate = str(proxy_url or "").strip()
    if candidate.lower().startswith("socks://"):
        return f"socks5://{candidate[len('socks://'):]}"
    return candidate or None



def _parse_base_url(base_url: str):
    """``urlparse`` that tolerates a bare ``host[:port][/path]`` (no scheme)."""
    raw = (base_url or "").strip()
    return urlparse(raw if "://" in raw else f"//{raw}") if raw else None


def _hostname_of(parsed) -> str:
    return (parsed.hostname or "").lower().rstrip(".") if parsed else ""


def base_url_hostname(base_url: str) -> str:
    """Lowercased hostname for a base URL, or ``""`` if absent.

    Compare exact hostnames against provider hosts instead of substring-matching the raw URL:
    ``https://api.openai.com.example/v1`` or ``https://proxy.test/api.openai.com/v1`` would
    otherwise pass as native endpoints and mis-route api_mode and auth.
    """
    return _hostname_of(_parse_base_url(base_url))
