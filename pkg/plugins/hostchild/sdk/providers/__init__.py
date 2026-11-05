"""Model-provider profiles registered by plugin code running in this plugin host."""

from __future__ import annotations

import logging

from providers.base import ProviderProfile

logger = logging.getLogger(__name__)

_REGISTRY: dict[str, ProviderProfile] = {}
_ALIASES: dict[str, str] = {}
_SOURCES: dict[str, str] = {}
_PROVIDER_LIST_CACHE: list[ProviderProfile] | None = None
_REGISTRY_GENERATION = 0


def register_provider(profile: ProviderProfile) -> None:
    """Register a provider profile by name and aliases; a later registration of a name replaces it."""
    global _PROVIDER_LIST_CACHE, _REGISTRY_GENERATION
    _REGISTRY_GENERATION += 1
    _REGISTRY[profile.name] = profile
    _SOURCES[profile.name] = "plugin"
    for alias in profile.aliases:
        _ALIASES[alias] = profile.name
    _PROVIDER_LIST_CACHE = None


def provider_source(name: str) -> str | None:
    canonical = _ALIASES.get(name, name)
    return _SOURCES.get(canonical)


def get_provider_profile(name: str) -> ProviderProfile | None:
    """The profile registered under *name* or one of its aliases."""
    return _REGISTRY.get(_ALIASES.get(name, name))


def registry_generation() -> int:
    return _REGISTRY_GENERATION


def list_providers() -> list[ProviderProfile]:
    """Every registered profile, one per canonical name."""
    global _PROVIDER_LIST_CACHE
    if _PROVIDER_LIST_CACHE is None:
        seen: set[int] = set()
        cache: list[ProviderProfile] = []
        for profile in _REGISTRY.values():
            if id(profile) not in seen:
                seen.add(id(profile))
                cache.append(profile)
        _PROVIDER_LIST_CACHE = cache
    return list(_PROVIDER_LIST_CACHE)
