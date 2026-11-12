"""Item kinds, field names, OTP and origin helpers and the item metadata of the Icebath vault store that the plugin SDK ships.

A login backend plugin returns :class:`VaultItemMeta` values; the encrypted store itself stays in the
Icebath process and is never opened by the plugin host.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any, Optional
from urllib.parse import urlsplit

VAULT_KINDS = ("login", "payment", "address")

LOGIN_IDENTIFIER_TYPES = ("email", "phone", "username")

# Canonical secret-payload fields per non-login kind. Each maps to the WHATWG autocomplete token the
# browser fill targets (agent/vault_login_classifier.py); the Desktop Add dialog and `icebath vault add`
# both write these names, so the fill never has to guess a user's ad-hoc field naming.
PAYMENT_FIELDS = {
    "card_number": "cc-number", "cardholder_name": "cc-name", "exp_month": "cc-exp-month",
    "exp_year": "cc-exp-year", "cvc": "cc-csc", "billing_postal_code": "postal-code",
}
ADDRESS_FIELDS = {
    "address_line1": "address-line1", "address_line2": "address-line2", "city": "address-level2",
    "state": "address-level1", "postal_code": "postal-code", "country": "country-name",
}
REQUIRED_FIELDS = {"payment": ("card_number", "exp_month", "exp_year", "cvc"),
                   "address": ("address_line1", "city", "postal_code", "country")}

_DEFAULT_PORTS = {"http": 80, "https": 443}


class VaultError(Exception):
    """Vault failure that is safe to surface (never contains secret values)."""


_OTP_ALGOS = {"SHA1": "sha1", "SHA256": "sha256", "SHA512": "sha512"}


def normalize_otp_secret(value: str) -> str:
    """Accept a raw base32 seed or an ``otpauth://totp/...`` URI. Returns the canonical stored form:
    the bare uppercase base32 seed, followed by ``|digits|period|algo`` ONLY when the URI departs from
    the RFC 6238 defaults (6 / 30 / SHA1), so a plain seed stays a plain seed. Non-default parameters
    are honoured, not dropped: an 8-digit or 60-second authenticator would otherwise get wrong codes."""
    value = (value or "").strip()
    if not value:
        return ""
    digits, period, algo = 6, 30, "SHA1"
    if value.lower().startswith("otpauth://"):
        from urllib.parse import parse_qs, urlparse
        parsed = urlparse(value)
        if parsed.netloc.lower() != "totp":
            raise VaultError("only otpauth://totp links are supported (counter-based HOTP is not)")
        qs = {k.lower(): v[0] for k, v in parse_qs(parsed.query).items()}
        value = qs.get("secret", "")
        try:
            digits = int(qs.get("digits", digits))
            period = int(qs.get("period", period))
        except ValueError:
            raise VaultError("otpauth:// digits/period must be integers")
        algo = qs.get("algorithm", algo).upper().replace("-", "")
        if digits not in (6, 7, 8) or period <= 0 or algo not in _OTP_ALGOS:
            raise VaultError("unsupported otpauth:// parameters (digits 6-8, period > 0, SHA1/SHA256/SHA512)")
    seed = re.sub(r"[\s-]", "", value).upper().rstrip("=")
    if not seed or re.search(r"[^A-Z2-7]", seed):
        raise VaultError("authenticator key must be a base32 secret or an otpauth:// URI")
    if (digits, period, algo) == (6, 30, "SHA1"):
        return seed
    return f"{seed}|{digits}|{period}|{algo}"


def totp_now(seed: str, *, digits: int = 6, period: int = 30, at: Optional[float] = None) -> str:
    """RFC 6238 TOTP for a stored seed (see normalize_otp_secret for the ``seed|digits|period|algo``
    form). Stdlib only."""
    import base64
    import hashlib
    import hmac
    import struct
    import time as _time

    algo = "sha1"
    if "|" in seed:
        seed, d, p, a = seed.split("|", 3)
        digits, period, algo = int(d), int(p), _OTP_ALGOS.get(a.upper(), "sha1")
    key = base64.b32decode(seed + "=" * (-len(seed) % 8), casefold=True)
    counter = int((at if at is not None else _time.time()) // period)
    digest = hmac.new(key, struct.pack(">Q", counter), getattr(hashlib, algo)).digest()
    offset = digest[-1] & 0x0F
    code = (struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF) % (10 ** digits)
    return str(code).zfill(digits)


def normalize_origin(url_or_origin: str) -> str:
    """Normalize a URL or origin to ``scheme://host[:port]``.

    Default ports (80 for http, 443 for https) are stripped so that
    ``https://example.com`` and ``https://example.com:443`` compare equal.
    Raises :class:`VaultError` for values without a scheme + host.
    """
    value = (url_or_origin or "").strip()
    if not value:
        raise VaultError("origin is required")
    if "://" not in value:
        raise VaultError(f"origin must include a scheme (got {value!r})")
    parts = urlsplit(value)
    scheme = (parts.scheme or "").lower()
    host = (parts.hostname or "").lower()
    if not scheme or not host:
        raise VaultError(f"could not parse origin from {value!r}")
    try:
        port = parts.port
    except ValueError as exc:
        raise VaultError(f"invalid port in origin {value!r}") from exc
    if port is None or port == _DEFAULT_PORTS.get(scheme):
        return f"{scheme}://{host}"
    return f"{scheme}://{host}:{port}"


@dataclass(frozen=True)
class VaultItemMeta:
    """Metadata-only view of a vault item. Never contains secret values.

    For ``kind='login'`` the identifier (email/username/phone) is metadata,
    not a secret: the agent may see it and type it itself. Only the password
    is vault-secret.
    """

    id: str
    kind: str
    label: str
    origin: Optional[str]
    created_at: str
    identifier_type: Optional[str] = None
    identifier: Optional[str] = None
    has_otp: bool = False  # a TOTP seed is stored: 2FA codes can be minted without asking the user
    # Every origin the password manager bound to this item (manager backends only;
    # ``origin`` is the first/primary one). Fill matching stays exact-origin against
    # this list — no wildcard or subdomain inference is ever derived from it.
    allowed_origins: tuple = ()

    def to_dict(self) -> dict[str, Any]:
        out = {
            "id": self.id,
            "kind": self.kind,
            "label": self.label,
            "origin": self.origin,
            "created_at": self.created_at,
        }
        if self.identifier is not None:
            out["identifier"] = self.identifier
            out["identifier_type"] = self.identifier_type
        if self.has_otp:
            out["has_otp"] = True
        if len(self.allowed_origins) > 1:
            out["allowed_origins"] = list(self.allowed_origins)
        return out


def scrub_secret_from_text(text: str, secret: dict[str, Any]) -> str:
    """Defensively strip any secret values from a string (e.g. an exception
    message) before it can be surfaced. Case-sensitive exact substring scrub."""
    scrubbed = text
    for value in secret.values():
        if isinstance(value, str) and len(value) >= 3 and value in scrubbed:
            scrubbed = scrubbed.replace(value, "[REDACTED]")
    # Also collapse anything that looks like a leaked password-ish token in
    # common key=value echoes.
    scrubbed = re.sub(r"(password['\"]?\s*[:=]\s*)\S+", r"\1[REDACTED]", scrubbed)
    return scrubbed
