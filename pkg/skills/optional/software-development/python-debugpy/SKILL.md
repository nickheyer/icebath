---
name: python-debugpy
description: "Debug Python: pdb REPL + debugpy remote (DAP)."
version: 1.0.0
author: icebath
license: MIT
platforms: [linux, macos]
metadata:
  icebath:
    tags: [debugging, python, pdb, debugpy, breakpoints, dap, post-mortem]
    related_skills: [systematic-debugging, node-inspect-debugger]
---

# Python Debugger (pdb + debugpy)

## Overview

Three tools, picked by situation:

| Tool | When |
|---|---|
| **`breakpoint()` + pdb** | Local, interactive, simplest. Add `breakpoint()` in the source, run normally, get a REPL at that line. |
| **`python -m pdb`** | Launch an existing script under pdb with no source edits. Useful for quick poking. |
| **`debugpy`** | Remote / headless / "attach to already-running process." Talks DAP, scriptable from terminal, works for long-lived processes (gateway, daemon, PTY children). |

**Start with `breakpoint()`.** It's the cheapest thing that works.

## When to Use

- A test fails and the traceback doesn't reveal why a value is wrong
- You need to step through a function and watch a collection mutate
- A long-running Python process (a service, a daemon, a worker a skill helper started) misbehaves and you can't restart it
- Post-mortem: an exception fired in prod-ish code and you want to inspect locals at the crash site
- A subprocess / child (a `multiprocessing` worker, a PTY child, a Python script Icebath ran through `terminal` or `execute_code`) is the actual bug site

**Don't use for:** things `print()` / `logging.debug` solve in under a minute, or things `pytest -vv --tb=long --showlocals` already reveals.

## pdb Quick Reference

Inside any pdb prompt (`(Pdb)`):

| Command | Action |
|---|---|
| `h` / `h cmd` | help |
| `n` | next line (step over) |
| `s` | step into |
| `r` | return from current function |
| `c` | continue |
| `unt N` | continue until line N |
| `j N` | jump to line N (same function only) |
| `l` / `ll` | list source around current line / full function |
| `w` | where (stack trace) |
| `u` / `d` | move up / down in the stack |
| `a` | print args of the current function |
| `p expr` / `pp expr` | print / pretty-print expression |
| `display expr` | auto-print expr on every stop |
| `b file:line` | set breakpoint |
| `b func` | break on function entry |
| `b file:line, cond` | conditional breakpoint |
| `cl N` | clear breakpoint N |
| `tbreak file:line` | one-shot breakpoint |
| `!stmt` | execute arbitrary Python (assignments included) |
| `interact` | drop into full Python REPL in current scope (Ctrl+D to exit) |
| `q` | quit |

The `interact` command is the most powerful — you can import anything, inspect complex objects, even call methods that mutate state. Locals are read-only by default; use `!x = 42` from the `(Pdb)` prompt to mutate.

## Recipe 1: Local breakpoint

Easiest. Edit the file:

```python
def compute(x, y):
    result = some_helper(x)
    breakpoint()           # <-- drops into pdb here
    return result + y
```

Run the code normally. You land at the `breakpoint()` line with full access to locals.

**Don't forget to remove `breakpoint()` before committing.** Use `git diff` or a pre-commit grep:
```bash
rg -n 'breakpoint\(\)' --type py
```

## Recipe 2: Launch a script under pdb (no source edits)

```bash
python -m pdb path/to/script.py arg1 arg2
# Lands at first line of script
(Pdb) b path/to/script.py:42
(Pdb) c
```

## Recipe 3: Debug a pytest test

Use `terminal` and the project's canonical test command for noninteractive diagnostics:

```bash
# Show locals in tracebacks without pdb:
.venv/bin/python -m pytest tests/path/to/test_file.py --showlocals --tb=long
```

A wrapper that captures each file in a separate subprocess (pytest-xdist, a
project `run_tests` script) cannot provide an interactive prompt for `--pdb`
or `--trace`. For an interactive debugger only, run pytest directly with the
project's development interpreter prepared in Recipe 5 (never an environment
Icebath owns):

```bash
.venv/bin/python -m pytest tests/foo_test.py::test_bar --pdb
```

This bypasses the wrapper's hermetic-env guarantees — fine for debugging, but re-run under the wrapper to confirm before pushing.

## Recipe 4: Post-mortem on any exception

```python
import pdb, sys
try:
    run_the_thing()
except Exception:
    pdb.post_mortem(sys.exc_info()[2])
```

Or wrap a whole script:

```bash
python -m pdb -c continue script.py
# When it crashes, pdb catches it and you're in the frame of the exception
```

Or set a global hook in a repl/jupyter:

```python
import sys
def excepthook(etype, value, tb):
    import pdb; pdb.post_mortem(tb)
sys.excepthook = excepthook
```

## Recipe 5: Remote debug with debugpy (attach to running process)

For long-lived processes: a web service, a queue worker, a daemon, a process that's already misbehaving and can't be restarted clean.

### Setup

Debug the target in its project's own development environment, with debugpy
declared in that project's development dependencies and installed through its
package manager. Through `terminal`, build a fresh, caller-owned debug/test
environment for a project that has none:

```bash
python3 -m venv .venv
.venv/bin/python -m pip install 'debugpy>=1.8,<2' -e .
.venv/bin/python -c "import debugpy; print(debugpy.__file__)"
```

For a stdlib-only script (a skill helper, a one-file tool) Icebath can build a
side environment with debugpy and print its interpreter:
`icebath pm environment debugpy --requirement 'debugpy>=1.8,<2'`.
`.venv/bin/python` below is this explicitly built debug environment, not a
guessed application venv, and the patterns below run through it. Never install
debugpy into an environment Icebath owns (its own dependency environment or
another `icebath pm environment` one), and do not add it to a running
production environment; reproduce there only with an already-prepared debug
target or arrange a restart in the development environment.

### Pattern A: Source-edit — process waits for debugger at launch

Add near the top of the entry point (or inside the function you want to debug):

```python
import debugpy
debugpy.listen(("127.0.0.1", 5678))
print("debugpy listening on 5678, waiting for client...", flush=True)
debugpy.wait_for_client()
debugpy.breakpoint()       # optional: pause immediately once attached
```

Start the process; it blocks on `wait_for_client()`.

### Pattern B: No source edit — launch with `-m debugpy`

```bash
.venv/bin/python -m debugpy --listen 127.0.0.1:5678 --wait-for-client your_script.py arg1
```

Equivalent for module entry:

```bash
.venv/bin/python -m debugpy --listen 127.0.0.1:5678 --wait-for-client -m your.module
```

### Pattern C: Attach to an already-running process

Needs the PID and debugpy preinstalled in the target's environment:

```bash
.venv/bin/python -m debugpy --listen 127.0.0.1:5678 --pid <pid>
# debugpy injects itself into the process. Then attach a client as below.
```

Some kernels/security configs block the ptrace-based injection (`/proc/sys/kernel/yama/ptrace_scope`). Fix with:
```bash
echo 0 | sudo tee /proc/sys/kernel/yama/ptrace_scope
```

### Connecting a client from the terminal

The easiest terminal-side DAP client is VS Code CLI or a small script. From inside Icebath you have two practical options:

**Option 1: `debugpy`'s own CLI REPL** — not an official feature, but a tiny DAP client script:

```python
# ~/.icebath/cache/scratch/dap_client.py
import socket, json, itertools, time, sys

HOST, PORT = "127.0.0.1", 5678
s = socket.create_connection((HOST, PORT))
seq = itertools.count(1)

def send(msg):
    msg["seq"] = next(seq)
    body = json.dumps(msg).encode()
    s.sendall(f"Content-Length: {len(body)}\r\n\r\n".encode() + body)

def recv():
    header = b""
    while b"\r\n\r\n" not in header:
        header += s.recv(1)
    length = int(header.decode().split("Content-Length:")[1].split("\r\n")[0].strip())
    body = b""
    while len(body) < length:
        body += s.recv(length - len(body))
    return json.loads(body)

send({"type": "request", "command": "initialize", "arguments": {"adapterID": "python"}})
print(recv())
send({"type": "request", "command": "attach", "arguments": {}})
print(recv())
send({"type": "request", "command": "setBreakpoints",
      "arguments": {"source": {"path": sys.argv[1]},
                    "breakpoints": [{"line": int(sys.argv[2])}]}})
print(recv())
send({"type": "request", "command": "configurationDone"})
# ... loop reading events and sending continue/stepIn/etc.
```

This is fine for one-off automation but painful as an interactive UX.

**Option 2: Attach from VS Code / Cursor / Zed** — if the user has one open, they can add a `launch.json`:

```json
{
  "name": "Attach to Python process",
  "type": "debugpy",
  "request": "attach",
  "connect": { "host": "127.0.0.1", "port": 5678 },
  "justMyCode": false,
  "pathMappings": [
    { "localRoot": "${workspaceFolder}", "remoteRoot": "<project-root>" }
  ]
}
```

**Option 3: Ditch DAP, use `remote-pdb`** — usually what you actually want from a terminal agent:

For an independently owned Python project, declare `remote-pdb` in that
project's development dependencies and prepare its debug environment through
the project's package manager. For a stdlib-only script,
`icebath pm environment remote-pdb --requirement 'remote-pdb>=2.1,<3'` prints a
Python that has it. The remote-pdb examples below require that separately
declared, freshly built debug environment, never an in-place pip install into
an environment Icebath owns.

In your code:
```python
from remote_pdb import set_trace
set_trace(host="127.0.0.1", port=4444)   # blocks until connection
```

Then from the terminal:
```bash
nc 127.0.0.1 4444
# You get a (Pdb) prompt exactly as if debugging locally.
```

`remote-pdb` is the cleanest agent-friendly choice when `debugpy`'s DAP protocol is overkill. Use `debugpy` only when you actually need IDE integration.

## Debugging the Python Processes Icebath Starts

Icebath itself is a Go binary, so pdb and debugpy cannot attach to it; debug it with Delve: `dlv attach <pid>` for a running `icebath`, or from a checkout `dlv debug --build-flags='-tags goolm' ./cmd/icebath -- chat -q '...'`. The Python it runs is ordinary Python and every recipe above applies to it.

### Tests
See Recipe 3. A capturing wrapper hides the prompt, so run pytest directly for interactive pdb.

### Skill helper scripts (`scripts/*.py` run through `terminal`)
Run the helper with the interpreter the skill selected (its `"$PY"`), under
pdb or debugpy, from a `terminal` session started with `pty=true` so the prompt
is reachable through `process_manage`:

```bash
"$PY" -m pdb scripts/the_helper.py arg1
"$PY" -m debugpy --listen 127.0.0.1:5678 --wait-for-client scripts/the_helper.py arg1
```

The second form needs debugpy in that interpreter; for a side environment Icebath
built, rebuild it under another name with debugpy added as one more `--requirement`.

### `execute_code` children
`execute_code` runs its code in a child Python process with no terminal, whose
standard input ends after a fixed payload, so `breakpoint()` there reads end of
input and pdb ends the process. Use `remote-pdb`
instead, with remote-pdb importable by that interpreter (the active venv or
conda Python in project mode, else the `python3` on PATH):

```python
from remote_pdb import set_trace
set_trace(host="127.0.0.1", port=4444)   # the code blocks here until you connect
```

Connect with `nc 127.0.0.1 4444` from a `terminal` call started with `background=true`.

### Long-lived services the agent started
A server or worker started with `terminal(background=true)` is a normal
process: attach with Pattern C, or restart it under Pattern B with `--wait-for-client`.

## Common Pitfalls

1. **pdb under a parallel/output-capturing runner silently does nothing.** You won't see the prompt, the test just hangs (true of pytest-xdist and of any wrapper that runs each file in a captured subprocess). Run pytest directly on a single file for interactive debugging.

2. **`breakpoint()` in CI / non-TTY contexts hangs the process.** Safe locally; never commit it. Add a pre-commit grep as a safety net.

3. **`PYTHONBREAKPOINT=0`** disables all `breakpoint()` calls. Check the env if your breakpoint isn't hitting:
   ```bash
   echo $PYTHONBREAKPOINT
   ```

4. **`debugpy.listen` blocks only if you also call `wait_for_client()`.** Without it, execution continues and your first breakpoint may fire before the client is attached.

5. **Attach to PID fails on hardened kernels.** `ptrace_scope=1` (Ubuntu default) allows only same-user ptrace of child processes. Workaround: `echo 0 > /proc/sys/kernel/yama/ptrace_scope` (needs root) or launch under `debugpy` from the start.

6. **Threads.** `pdb` only debugs the current thread. For multithreaded code, use `debugpy` (thread-aware DAP) or set `threading.settrace()` per thread.

7. **asyncio.** `pdb` works in coroutines but `await` inside pdb requires Python 3.13+ or `await` from `interact` mode on older versions. For 3.11/3.12, use `asyncio.run_coroutine_threadsafe` tricks or `!stmt`-based awaits via `asyncio.ensure_future`.

8. **Hermetic test wrappers strip credentials and set `HOME=<tmpdir>`.** If your bug depends on user config or real API keys, it won't reproduce under such a wrapper. Debug with raw `pytest` first to repro, then re-confirm under the wrapper.

9. **Forking / multiprocessing.** pdb does not follow forks. Each child needs its own `breakpoint()` or `set_trace()`. For several Python children of one agent turn (parallel `execute_code` or background `terminal` calls), debug one process at a time.

## Verification Checklist

- [ ] In the independently built debug environment, confirm: `.venv/bin/python -c "import debugpy; print(debugpy.__version__); print(debugpy.__file__)"`
- [ ] For remote debug, confirm the port is actually listening: `ss -tlnp | grep 5678`
- [ ] First breakpoint actually hits (if it doesn't, you likely have `PYTHONBREAKPOINT=0`, you're under a parallel/capturing runner, or execution finished before attach)
- [ ] `where` / `w` shows the expected call stack
- [ ] Post-debug cleanup: no stray `breakpoint()` / `set_trace()` in committed code
  ```bash
  rg -n 'breakpoint\(\)|set_trace\(|debugpy\.listen' --type py
  ```

## One-Shot Recipes

**"Why is this dict missing a key?"**
```python
# add above the KeyError site
breakpoint()
# then in pdb:
(Pdb) pp d
(Pdb) pp list(d.keys())
(Pdb) w                # how did we get here
```

**"This test passes in isolation but fails in the suite."**
```bash
.venv/bin/python -m pytest tests/the_test.py   # confirm it fails on its own first
# For interactive debugging, or if it only fails WITH other tests, use the
# independent development/test interpreter prepared in Recipe 5:
.venv/bin/python -m pytest tests/ -x --pdb
# Now it pdb-traps at the exact failing test after state accumulated.
```

**"My async handler deadlocks."**
```python
# Add at handler entry
import remote_pdb; remote_pdb.set_trace(host="127.0.0.1", port=4444)
```
Trigger the handler. `nc 127.0.0.1 4444`, then `w` to see the suspended frame, `!import asyncio; asyncio.all_tasks()` to see what else is pending.

**"Post-mortem on a crash in a child process / subprocess."**
```bash
PYTHONFAULTHANDLER=1 python -m pdb -c continue path/to/entrypoint.py
# On crash, pdb lands at the frame of the exception with full locals
```
