"""Auto-generated Icebath tools RPC stubs (file-based transport)."""
import json, os, shlex, tempfile, threading, time

_RPC_DIR = os.environ.get("ICEBATH_RPC_DIR") or os.path.join(tempfile.gettempdir(), "icebath_rpc")
_seq = 0
# `_seq += 1` is not atomic (read-modify-write), so concurrent _call()
# invocations from multiple threads could allocate the same sequence number
# and clobber each other's request files. Guard seq allocation with a lock.
_seq_lock = threading.Lock()

# ---------------------------------------------------------------------------
# Convenience helpers (avoid common scripting pitfalls)
# ---------------------------------------------------------------------------

def json_parse(text: str):
    """Parse JSON tolerant of control characters and UTF-8 BOM (strict=False).
    Use this instead of json.loads() when parsing output from terminal()
    or web_extract() that may contain raw tabs/newlines in strings,
    or from tools/files that prepend a UTF-8 BOM (salvage #57870, credit @woxinwuhen713-bit)."""
    if isinstance(text, str) and text.startswith("﻿"):
        text = text[1:]
    return json.loads(text, strict=False)


def shell_quote(s: str) -> str:
    """Shell-escape a string for safe interpolation into commands.
    Use this when inserting dynamic content into terminal() commands:
        terminal(f"echo {shell_quote(user_input)}")
    """
    return shlex.quote(s)


def retry(fn, max_attempts=3, delay=2):
    """Retry a function up to max_attempts times with exponential backoff.
    Use for transient failures (network errors, API rate limits):
        result = retry(lambda: terminal("gh issue list ..."))
    """
    last_err = None
    for attempt in range(max_attempts):
        try:
            return fn()
        except Exception as e:
            last_err = e
            if attempt < max_attempts - 1:
                time.sleep(delay * (2 ** attempt))
    raise last_err


def _call(tool_name, args):
    """Send a tool call request via file-based RPC and wait for response."""
    global _seq
    with _seq_lock:
        _seq += 1
        seq = _seq
    seq_str = f"{seq:06d}"
    req_file = os.path.join(_RPC_DIR, f"req_{seq_str}")
    res_file = os.path.join(_RPC_DIR, f"res_{seq_str}")

    # Write request atomically (write to .tmp, then rename).
    # encoding="utf-8" is critical: on Windows-hosted remote backends
    # (or any non-UTF-8 locale) the default open() mode would mangle
    # non-ASCII chars in tool args when encoding them as JSON.
    tmp = req_file + ".tmp"
    # The request carries the RPC token and tool args: owner-only even under a
    # permissive process umask.
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump({
            "tool": tool_name,
            "args": args,
            "seq": seq,
            "token": os.environ.get("ICEBATH_RPC_TOKEN", ""),
        }, f)
    os.rename(tmp, req_file)

    # Wait for response with adaptive polling
    deadline = time.monotonic() + 300  # 5-minute timeout per tool call
    poll_interval = 0.05  # Start at 50ms
    while not os.path.exists(res_file):
        if time.monotonic() > deadline:
            raise RuntimeError(f"RPC timeout: no response for {tool_name} after 300s")
        time.sleep(poll_interval)
        poll_interval = min(poll_interval * 1.2, 0.25)  # Back off to 250ms

    with open(res_file, encoding="utf-8") as f:
        raw = f.read()

    # Clean up response file
    try:
        os.unlink(res_file)
    except OSError:
        pass

    result = json.loads(raw)
    if isinstance(result, str):
        try:
            return json.loads(result)
        except (json.JSONDecodeError, TypeError):
            return result
    return result

