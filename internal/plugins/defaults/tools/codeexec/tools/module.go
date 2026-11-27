package tools

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Tools a script may call, in the order the schema lists them, cut to those the session offers
var sandboxTools = []string{"web_search", "web_extract", "read_file", "write_file", "search_files", "patch", "terminal"}

// Signature, docstring and arguments of each tool's function in the generated module
var stubs = map[string]struct{ sig, doc, args string }{
	"web_search": {"query: str, limit: int = 5",
		`"""Search the web. Returns dict with data.web list of {url, title, description}."""`,
		`{"query": query, "limit": limit}`},
	"web_extract": {"urls: list, char_limit: int = None",
		`"""Extract content from URLs (no LLM summarization). Returns dict with results list of {url, title, content, error}. Pages over char_limit (default 15000) are head+tail truncated with the full text stored on disk; the content footer gives the path. content is markdown."""`,
		`{"urls": urls, "char_limit": char_limit}`},
	"read_file": {"path: str, offset: int = 1, limit: int = 2000",
		`"""Read a file (1-indexed lines). Returns dict with "content" and "total_lines"."""`,
		`{"path": path, "offset": offset, "limit": limit}`},
	"write_file": {"path: str, content: str, cross_profile: bool = False",
		`"""Write content to a file (always overwrites). Returns dict with status."""`,
		`{"path": path, "content": content, "cross_profile": cross_profile}`},
	"search_files": {`pattern: str, target: str = "content", path: str = ".", file_glob: str = None, limit: int = 50, offset: int = 0, output_mode: str = "content", context: int = 0, order: str = "discovery"`,
		`"""Search file contents (target="content") or find files by name (target="files"). Returns dict with "matches"."""`,
		`{"pattern": pattern, "target": target, "path": path, "file_glob": file_glob, "limit": limit, "offset": offset, "output_mode": output_mode, "context": context, "order": order}`},
	"patch": {`path: str = None, old_string: str = None, new_string: str = None, replace_all: bool = False, mode: str = "replace", patch: str = None, cross_profile: bool = False`,
		`"""Targeted find-and-replace (mode="replace") or V4A multi-file patches (mode="patch"). Returns dict with status."""`,
		`{"path": path, "old_string": old_string, "new_string": new_string, "replace_all": replace_all, "mode": mode, "patch": patch, "cross_profile": cross_profile}`},
	"terminal": {"command: str, timeout: int = None, workdir: str = None",
		`"""Run a shell command (foreground only). Returns dict with "output" and "exit_code"."""`,
		`{"command": command, "timeout": timeout, "workdir": workdir}`},
}

// Lines of each tool the schema documents
var docs = []struct{ name, doc string }{
	{"web_search", "  web_search(query: str, limit: int = 5) -> dict\n" +
		"    Returns {\"data\": {\"web\": [{\"url\", \"title\", \"description\"}, ...]}}"},
	{"web_extract", "  web_extract(urls: list[str], char_limit: int = None) -> dict\n" +
		"    Returns {\"results\": [{\"url\", \"title\", \"content\", \"error\"}, ...]} where content is markdown.\n" +
		"    No LLM summarization. Pages over char_limit (default 15000) are head+tail truncated; full text stored on disk (path in the content footer)."},
	{"read_file", "  read_file(path: str, offset: int = 1, limit: int = 2000) -> dict\n" +
		"    Lines are 1-indexed. Returns {\"content\": \"...\", \"total_lines\": N}"},
	{"write_file", "  write_file(path: str, content: str) -> dict\n    Always overwrites the entire file; an existing file must be read_file'd first or the write is refused."},
	{"search_files", "  search_files(pattern: str, target=\"content\", path=\".\", file_glob=None, limit=50, order=\"discovery\") -> dict\n" +
		"    target: \"content\" (search inside files) or \"files\" (find files by name). Returns {\"matches\": [...]}"},
	{"patch", "  patch(path: str, old_string: str, new_string: str, replace_all: bool = False) -> dict\n" +
		"    Replaces old_string with new_string in the file."},
	{"terminal", "  terminal(command: str, timeout=None, workdir=None) -> dict\n" +
		"    Foreground only (no background/pty). Returns {\"output\": \"...\", \"exit_code\": N}"},
}

// Helpers both transports of the module carry
const helpers = `
# ---------------------------------------------------------------------------
# Convenience helpers (avoid common scripting pitfalls)
# ---------------------------------------------------------------------------

def json_parse(text: str):
    """Parse JSON tolerant of control characters and UTF-8 BOM (strict=False).
    Use this instead of json.loads() when parsing output from terminal()
    or web_extract() that may contain raw tabs/newlines in strings,
    or from tools/files that prepend a UTF-8 BOM."""
    if isinstance(text, str) and text.startswith("` + "\ufeff" + `"):
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

`

// Module header calling the tools over the socket of this machine's RPC server
const socketHeader = `"""Auto-generated Icebath tools RPC stubs."""
import json, os, socket, shlex, threading, time

_sock = None
# The RPC server handles a single client connection serially and has no
# request-id in the protocol, so concurrent _call() invocations from multiple
# threads (e.g. ThreadPoolExecutor) would race on the shared socket and get
# each other's responses. Serialize the entire send+recv round-trip.
_call_lock = threading.Lock()
` + helpers + `
def _connect():
    """Connect to the parent's RPC server via the transport it picked.

    ICEBATH_RPC_SOCKET can be either:
      - a filesystem path (POSIX Unix domain socket, the default on
        Linux and macOS)
      - a string of the form tcp://127.0.0.1:<port> (Windows, where
        AF_UNIX is unreliable, so the parent listens on loopback TCP)
    """
    global _sock
    if _sock is None:
        endpoint = os.environ["ICEBATH_RPC_SOCKET"]
        if endpoint.startswith("tcp://"):
            _host_port = endpoint[len("tcp://"):]
            _host, _, _port = _host_port.rpartition(":")
            _sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            _sock.connect((_host or "127.0.0.1", int(_port)))
        else:
            _sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            _sock.connect(endpoint)
        _sock.settimeout(300)
    return _sock

def _call(tool_name, args):
    """Send a tool call to the parent process and return the parsed result."""
    request = json.dumps({
        "tool": tool_name,
        "args": args,
        "token": os.environ.get("ICEBATH_RPC_TOKEN", ""),
    }) + "\n"
    # Session kernels outlive the RPC server's 300s idle window, so their
    # connection can be legitimately gone by the next cell. The server
    # re-accepts (ICEBATH_RPC_PERSISTENT=1); retry once on a fresh socket.
    _attempts = 2 if os.environ.get("ICEBATH_RPC_PERSISTENT") == "1" else 1
    with _call_lock:
        for _attempt in range(_attempts):
            try:
                conn = _connect()
                conn.sendall(request.encode())
                buf = b""
                while True:
                    chunk = conn.recv(65536)
                    if not chunk:
                        raise RuntimeError("Agent process disconnected")
                    buf += chunk
                    if buf.endswith(b"\n"):
                        break
                break
            except (OSError, RuntimeError):
                global _sock
                try:
                    if _sock is not None:
                        _sock.close()
                except OSError:
                    pass
                _sock = None
                if _attempt + 1 >= _attempts:
                    raise
    raw = buf.decode().strip()
    result = json.loads(raw)
    if isinstance(result, str):
        try:
            return json.loads(result)
        except (json.JSONDecodeError, TypeError):
            return result
    return result

`

// Module header calling the tools through request and response files, for a sandbox the host cannot open a socket into
const fileHeader = `"""Auto-generated Icebath tools RPC stubs (file-based transport)."""
import json, os, shlex, tempfile, threading, time

_RPC_DIR = os.environ.get("ICEBATH_RPC_DIR") or os.path.join(tempfile.gettempdir(), "icebath_rpc")
_seq = 0
# _seq += 1 is not atomic (read-modify-write), so concurrent _call()
# invocations from multiple threads could allocate the same sequence number
# and clobber each other's request files. Guard seq allocation with a lock.
_seq_lock = threading.Lock()
` + helpers + `
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
    # encoding="utf-8" keeps non-ASCII tool args intact under any locale.
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

`

// Source of the icebath_tools module with a function for each tool of the set, over sockets or files
func module(tools []string, files bool) string {
	header := socketHeader
	if files {
		header = fileHeader
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range stubs {
			if !yield(name) {
				return
			}
		}
	})
	var functions []string
	for _, name := range names {
		if slices.Contains(tools, name) {
			s := stubs[name]
			functions = append(functions, fmt.Sprintf("def %s(%s):\n    %s\n    return _call(%s, %s)\n", name, s.sig, s.doc, strconv.Quote(name), s.args))
		}
	}
	return header + strings.Join(functions, "\n")
}

// Runs one cell, shared by the runners of this machine and of a sandbox
const cellSource = `GLOBALS = {"__name__": "__main__", "__builtins__": __builtins__}


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
`

// Kernel of this machine: one cell per line of stdin, each reply framed by the sentinel on the real stdout
const kernelSource = `"""Auto-generated Icebath session-kernel runner. One exec cell per request."""
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

` + cellSource + `

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
`

// Kernel of a sandbox: one cell per request file, each result written beside it, gone by itself after the idle window
const remoteKernelSource = `"""Auto-generated Icebath REMOTE session-kernel runner (file cell protocol)."""
import contextlib
import io
import json
import os
import sys
import time
import traceback

KDIR = os.environ["ICEBATH_KERNEL_DIR"]
CELLS = os.path.join(KDIR, "cells")
_CAPTURE_LIMIT = {capture_limit}
IDLE_EXIT_SECONDS = {idle_exit}

{cell_source}

def main():
    execution_count = 0
    last_activity = time.time()
    while True:
        pending = sorted(
            f for f in os.listdir(CELLS)
            if f.startswith("cell_req_") and f.endswith(".json")
        )
        if not pending:
            if time.time() - last_activity > IDLE_EXIT_SECONDS:
                return  # self-reap: nobody is talking to us anymore
            time.sleep(0.2)
            continue
        for name in pending:
            req_path = os.path.join(CELLS, name)
            try:
                with open(req_path, "r", encoding="utf-8") as f:
                    request = json.load(f)
            except Exception:
                # Partially-written request (ship in progress): retry next tick.
                continue
            os.remove(req_path)
            last_activity = time.time()
            execution_count += 1
            payload, _ = run_cell(request, execution_count)
            res_name = name.replace("cell_req_", "cell_res_")
            tmp = os.path.join(CELLS, res_name + ".tmp")
            # Cell results carry the executed code's output: owner-only, even if
            # the process umask is permissive.
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(payload, f, ensure_ascii=False)
            os.replace(tmp, os.path.join(CELLS, res_name))
            if payload["status"] == "exit":
                return


if __name__ == "__main__":
    main()
`

// Source of the sandbox kernel runner with its capture limit and idle window
func remoteRunner(idle int) string {
	return strings.NewReplacer("{cell_source}", cellSource, "{capture_limit}", strconv.Itoa(maxStdout), "{idle_exit}", strconv.Itoa(idle)).
		Replace(remoteKernelSource)
}

// Hint for a name a script tried to import from icebath_tools that is not there
func missingImport(missing string, tools []string) string {
	if missing == "json_parse" || missing == "shell_quote" || missing == "retry" {
		return fmt.Sprintf("Import helpers with `from icebath_tools import %s`. If that import failed, the generated module may be stale or another "+
			"icebath_tools may be first on sys.path. Check icebath_tools.__file__ and retry with reset=true.", missing)
	}
	pool := tools
	if len(pool) == 0 {
		pool = sandboxTools
	}
	var available []string
	for _, name := range sandboxTools {
		if slices.Contains(pool, name) {
			available = append(available, name)
		}
	}
	slices.Sort(available)
	return fmt.Sprintf("'%s' is not available inside the execute_code sandbox. Importable tools here: %s. For anything else, use the normal "+
		"tool call instead of execute_code.", missing, strings.Join(available, ", "))
}

// Failures of scripts the model makes often, each with what to do instead
var failures = []struct {
	pattern *regexp.Regexp
	hint    func(group string, tools []string) string
}{
	{regexp.MustCompile(`cannot import name '(\w+)' from 'icebath_tools'`), missingImport},
	{regexp.MustCompile(`NameError: name '(json_parse|shell_quote|retry)' is not defined`), func(g string, _ []string) string {
		return fmt.Sprintf("Import %s before calling it: from icebath_tools import %s", g, g)
	}},
	{regexp.MustCompile(`ModuleNotFoundError: No module named '([\w.]+)'`), func(g string, _ []string) string {
		return fmt.Sprintf("'%s' is not installed in the sandbox interpreter. Use Python stdlib inside execute_code, or run the code via terminal() "+
			"with the project venv's python instead.", g)
	}},
	{regexp.MustCompile(`TypeError: string indices must be integers|AttributeError: 'str' object has no attribute 'get'`), func(string, []string) string {
		return "Tool functions in the sandbox return DICTS (already parsed) — do not json.loads() them or index them like strings. Example: " +
			"read_file(path)['content']."
	}},
}

// Hint for the first well known failure in the start of a traceback, empty when none matches
func failureHint(trace string, tools []string) string {
	if trace == "" {
		return ""
	}
	window := []rune(trace)
	window = window[:min(len(window), 4000)]
	for _, f := range failures {
		if m := f.pattern.FindStringSubmatch(string(window)); m != nil {
			group := ""
			if len(m) > 1 {
				group = m[1]
			}
			return f.hint(group, tools)
		}
	}
	return ""
}
