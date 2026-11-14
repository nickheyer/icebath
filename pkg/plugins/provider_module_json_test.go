package plugins

import ()

const jsonProviderSource = `import contextlib
import json
import threading
from pathlib import Path

EVENTS = []
_lock = threading.RLock()
_plain = threading.Lock()


def block(cfg, host):
    cfg.setdefault("seen", []).append(host)
    return (cfg.get("hosts") or {}).get(host) or {}


def swap(cfg, keep):
    old = cfg["hosts"]
    cfg["hosts"] = {"new": {}}
    if keep:
        cfg["old"] = old
        cfg["again"] = [old]
    return None


def where():
    return Path("/tmp") / "cfg.json"


def kind(value):
    return type(value).__name__


def strict(text):
    return json.loads(text)


@contextlib.contextmanager
def _guard(path, swallow):
    EVENTS.append(["enter", type(path).__name__, _plain.locked()])
    try:
        yield
    except ValueError as exc:
        EVENTS.append(["raised", str(exc)])
        if not swallow:
            raise
    finally:
        EVENTS.append(["exit"])


def state():
    return {"events": EVENTS, "plain": _plain.locked()}
`
