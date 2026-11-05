"""Per-model compression thresholds a plugin context engine can reuse."""

from __future__ import annotations


def _model_threshold_key_rank(key: str, model: str, provider: str) -> tuple[int, int] | None:
    """Match rank for one ``model_thresholds`` key, or None when it does not apply.
    ``"<provider>:<substr>"`` keys apply only on that provider; bare keys apply on every route.
    The same slug means different windows on different routes (Codex caps Astra at 272K; OpenRouter
    serves the full window), so a bare ``astra: 0.85`` written for Codex silently leaks everywhere.
    Rank = (substring length, scoped): the most specific model match wins, scope breaks ties."""
    scope, sep, substr = key.partition(":")
    if not sep:
        return (len(key), 0) if key in model else None
    return (len(substr), 1) if scope.strip().lower() == provider and substr in model else None


def resolve_model_threshold(
    model: str, model_thresholds: dict[str, float] | None, default: float, provider: str = "",
) -> float:
    """Per-model threshold: longest matching ``model_thresholds`` key wins, else ``default``.
    Keys are substrings of the model name, optionally provider-scoped as ``"<provider>:<substr>"``
    (a scoped key outranks a bare one of the same substring). Module-level so plugin context
    engines can reuse it."""
    if not model_thresholds or not model:
        return default
    provider = (provider or "").strip().lower()
    ranked = ((_model_threshold_key_rank(key, model, provider), key) for key in model_thresholds)
    best = max(((rank, key) for rank, key in ranked if rank is not None), default=None)
    return float(model_thresholds[best[1]]) if best else default
