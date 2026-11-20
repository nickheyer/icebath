package codeexec

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Tools allowed inside the sandbox, intersected with the session's enabled tools to pick the generated stubs
var sandboxAllowedTools = []string{"web_search", "web_extract", "read_file", "write_file", "search_files", "patch", "terminal"}

type toolStub struct {
	sig, doc, argsExpr string
}

var toolStubs = map[string]toolStub{
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

// Convenience helpers appended to both tool transport headers
const commonHelpers = `
# ---------------------------------------------------------------------------
# Convenience helpers (avoid common scripting pitfalls)
# ---------------------------------------------------------------------------

def json_parse(text: str):
    """Parse JSON tolerant of control characters and UTF-8 BOM (strict=False).
    Use this instead of json.loads() when parsing output from terminal()
    or web_extract() that may contain raw tabs/newlines in strings,
    or from tools/files that prepend a UTF-8 BOM (salvage #57870, credit @woxinwuhen713-bit)."""
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

// Tools module header that calls icebath tools over the local rpc socket
const udsTransportHeader = `"""Auto-generated Icebath tools RPC stubs."""
import json, os, socket, shlex, threading, time

_sock = None
# The RPC server handles a single client connection serially and has no
# request-id in the protocol, so concurrent _call() invocations from multiple
# threads (e.g. ThreadPoolExecutor) would race on the shared socket and get
# each other's responses. Serialize the entire send+recv round-trip.
_call_lock = threading.Lock()
` +
	commonHelpers +
	`
def _connect():
    """Connect to the parent's RPC server via the transport it picked.

    ICEBATH_RPC_SOCKET can be either:
      - a filesystem path (POSIX Unix domain socket — the default on
        Linux and macOS)
      - a string of the form ` + "`" + `` + "`" + `tcp://127.0.0.1:<port>` + "`" + `` + "`" + ` (Windows, where
        AF_UNIX is unreliable — the parent falls back to loopback TCP)
    """
    global _sock
    if _sock is None:
        endpoint = os.environ["ICEBATH_RPC_SOCKET"]
        if endpoint.startswith("tcp://"):
            # tcp://host:port  (host is always 127.0.0.1 in practice — we
            # only bind loopback server-side)
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

// Tools module header that calls icebath tools through request and response files
const fileTransportHeader = `"""Auto-generated Icebath tools RPC stubs (file-based transport)."""
import json, os, shlex, tempfile, threading, time

_RPC_DIR = os.environ.get("ICEBATH_RPC_DIR") or os.path.join(tempfile.gettempdir(), "icebath_rpc")
_seq = 0
# ` + "`" + `_seq += 1` + "`" + ` is not atomic (read-modify-write), so concurrent _call()
# invocations from multiple threads could allocate the same sequence number
# and clobber each other's request files. Guard seq allocation with a lock.
_seq_lock = threading.Lock()
` +
	commonHelpers +
	`
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

`

// Source of the icebath_tools wrapper module for the allowed and enabled tools over the uds or file transport
func generateToolsModule(enabledTools []string, transport string) string {
	header := udsTransportHeader
	if transport == "file" {
		header = fileTransportHeader
	}
	names := make([]string, 0, len(toolStubs))
	for name := range toolStubs {
		names = append(names, name)
	}
	sort.Strings(names)
	var stubs []string
	for _, name := range names {
		if !slices.Contains(enabledTools, name) {
			continue
		}
		s := toolStubs[name]
		stubs = append(stubs, fmt.Sprintf("def %s(%s):\n    %s\n    return _call(%s, %s)\n", name, s.sig, s.doc, pyrepr.StrRepr(name), s.argsExpr))
	}
	return header + strings.Join(stubs, "\n")
}

// Recovery hint for a name the script tried to import from icebath_tools
func missingToolsImportHint(missing string, enabledTools []string) string {
	if missing == "json_parse" || missing == "shell_quote" || missing == "retry" {
		return fmt.Sprintf("Import helpers with `from icebath_tools import %s`. "+
			"If that import failed, the generated module may be stale or another "+
			"icebath_tools may be first on sys.path. Check icebath_tools.__file__ "+
			"and retry with reset=true.", missing)
	}
	pool := enabledTools
	if len(pool) == 0 {
		pool = sandboxAllowedTools
	}
	var available []string
	for _, name := range sandboxAllowedTools {
		if slices.Contains(pool, name) {
			available = append(available, name)
		}
	}
	sort.Strings(available)
	return fmt.Sprintf("'%s' is not available inside the execute_code sandbox. "+
		"Importable tools here: %s. For anything "+
		"else, use the normal tool call instead of execute_code.", missing, strings.Join(available, ", "))
}

type failureHintRule struct {
	pattern *regexp2.Regexp
	format  func(group string, enabledTools []string) string
}

var failureHintRules = []failureHintRule{
	{regexp2.MustCompile(`cannot import name '(\w+)' from 'icebath_tools'`, regexp2.None), missingToolsImportHint},
	{regexp2.MustCompile(`NameError: name '(json_parse|shell_quote|retry)' is not defined`, regexp2.None),
		func(g string, _ []string) string {
			return fmt.Sprintf("Import %s before calling it: from icebath_tools import %s", g, g)
		}},
	{regexp2.MustCompile(`ModuleNotFoundError: No module named '([\w.]+)'`, regexp2.None),
		func(g string, _ []string) string {
			return fmt.Sprintf("'%s' is not installed in the sandbox interpreter. "+
				"Use Python stdlib inside execute_code, or run the code via "+
				"terminal() with the project venv's python instead.", g)
		}},
	{regexp2.MustCompile(`TypeError: string indices must be integers|AttributeError: 'str' object has no attribute 'get'`, regexp2.None),
		func(string, []string) string {
			return "Tool functions in the sandbox return DICTS (already parsed) — " +
				"do not json.loads() them or index them like strings. Example: read_file(path)['content']."
		}},
}

// One actionable recovery hint for a well known sandbox failure in the first 4000 characters, first match wins
func sandboxFailureHint(stderrText string, enabledTools []string) string {
	if stderrText == "" {
		return ""
	}
	window := clipRunes(stderrText, 4000)
	for _, rule := range failureHintRules {
		m, err := rule.pattern.FindStringMatch(window)
		if err != nil {
			return ""
		}
		if m != nil {
			group := ""
			if len(m.Groups()) > 1 {
				group = m.Groups()[1].String()
			}
			return rule.format(group, enabledTools)
		}
	}
	return ""
}
