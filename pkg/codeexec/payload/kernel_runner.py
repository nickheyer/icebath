"""Auto-generated Icebath session-kernel runner. One exec cell per request."""
import contextlib
import io
import json
import os
import sys
import threading
import traceback

_SENTINEL = os.environ["ICEBATH_KERNEL_SENTINEL"]
_CAPTURE_LIMIT = 1000000
_SPILL_DIR = os.environ.get("ICEBATH_KERNEL_SPILL_DIR", "")
_SPILL_CAP = 5000000
_PARENT_PROCESS_HANDLE = os.environ.pop("ICEBATH_KERNEL_PARENT_PROCESS_HANDLE", "")


def _start_parent_process_watchdog():
    """Exit when the exact Windows parent process object is signaled.

    The inherited SYNCHRONIZE handle names a process object, not a reusable
    PID. Missing or invalid handles fail open so watchdog setup can never kill
    an otherwise healthy kernel.
    """
    global _PARENT_PROCESS_HANDLE
    raw_handle = _PARENT_PROCESS_HANDLE
    _PARENT_PROCESS_HANDLE = ""
    if sys.platform != "win32" or not raw_handle:
        return
    try:
        import ctypes
        from ctypes import wintypes

        handle = int(raw_handle)
        if handle <= 0:
            return
        kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
        kernel32.WaitForSingleObject.argtypes = [wintypes.HANDLE, wintypes.DWORD]
        kernel32.WaitForSingleObject.restype = wintypes.DWORD
        kernel32.SetHandleInformation.argtypes = [
            wintypes.HANDLE,
            wintypes.DWORD,
            wintypes.DWORD,
        ]
        kernel32.SetHandleInformation.restype = wintypes.BOOL
        kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
        kernel32.CloseHandle.restype = wintypes.BOOL
        # This process needs the handle, but user code spawned by a cell must
        # not pass it any further. If Windows refuses to clear inheritance,
        # disable the watchdog rather than leak the handle into cell children.
        if not kernel32.SetHandleInformation(handle, 0x00000001, 0):
            kernel32.CloseHandle(handle)
            return
    except (ImportError, OSError, TypeError, ValueError):
        return

    def _wait():
        try:
            result = kernel32.WaitForSingleObject(handle, 0xFFFFFFFF)
        finally:
            kernel32.CloseHandle(handle)
        if result == 0x00000000:  # WAIT_OBJECT_0: the parent exited
            os._exit(0)

    threading.Thread(target=_wait, name="icebath-parent-watchdog", daemon=True).start()


_start_parent_process_watchdog()

_real_stdout = sys.stdout

GLOBALS = {"__name__": "__main__", "__builtins__": __builtins__}


def _clip(text):
    return (text, False) if len(text) <= _CAPTURE_LIMIT else (text[:_CAPTURE_LIMIT], True)


def run_cell(request, execution_count):
    """Exec one cell; returns (response payload, FULL stdout text)."""
    out, err = io.StringIO(), io.StringIO()
    status, trace = "ok", ""
    try:
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            exec(compile(request["code"], "<cell>", "exec"), GLOBALS)
    except SystemExit as exc:
        status, trace = "exit", "SystemExit: " + repr(exc.code)
    except BaseException:
        status, trace = "error", traceback.format_exc()
    stdout_text, stdout_clipped = _clip(out.getvalue())
    stderr_text, stderr_clipped = _clip(err.getvalue())
    return {
        "id": request.get("id", ""), "status": status,
        "stdout": stdout_text, "stderr": stderr_text,
        "stdout_clipped": stdout_clipped, "stderr_clipped": stderr_clipped,
        "traceback": trace, "execution_count": execution_count,
    }, out.getvalue()


def _spill(text, spill_name):
    """Best-effort: write the FULL clipped stdout to disk, return its path or ""."""
    if not _SPILL_DIR:
        return ""
    try:
        spill_path = os.path.join(_SPILL_DIR, spill_name)
        with open(spill_path, "w", encoding="utf-8", errors="replace") as f:
            f.write(text[:_SPILL_CAP])
            if len(text) > _SPILL_CAP:
                f.write("\n\n[... spill capped ...]")
        return spill_path
    except Exception:
        return ""


def _reply(payload):
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    _real_stdout.buffer.write(("\n" + _SENTINEL + " " + str(len(body)) + "\n").encode("utf-8"))
    _real_stdout.buffer.write(body)
    _real_stdout.buffer.flush()


def main():
    execution_count = 0
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            request = json.loads(line)
        except ValueError:
            continue
        execution_count += 1
        payload, full_stdout = run_cell(request, execution_count)
        payload["stdout_spill_path"] = (
            _spill(full_stdout, "cell_%06d_stdout.txt" % execution_count)
            if payload["stdout_clipped"] else ""
        )
        _reply(payload)
        if payload["status"] == "exit":
            break


if __name__ == "__main__":
    main()
