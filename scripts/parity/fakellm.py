import json, sys, time, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL = "fake-model"
DUMPN = [0]
LOG = open(sys.argv[2], "a") if len(sys.argv) > 2 else None
MARKDOWN = """# Heading One

Here is a paragraph with **bold text**, *italic text*, `inline code`, and a [link](https://example.com). It is long enough that it has to wrap across the terminal width at least once so wrapping behaviour is visible in the capture.

## Heading Two

- first bullet
- second bullet with `code`
  - nested bullet
1. numbered one
2. numbered two

```python
def fizzbuzz(n):
    for i in range(1, n + 1):
        print("fizzbuzz" if i % 15 == 0 else "fizz" if i % 3 == 0 else "buzz" if i % 5 == 0 else i)
```

| Column A | Column B | Column C |
|----------|----------|----------|
| a1       | b1       | c1       |
| a2       | b2       | c2       |

> a block quote line

Final line with unicode: café 日本語 ✓ and an emoji 🚀.
"""

def last_user(messages):
    for m in reversed(messages):
        if m.get("role") == "user":
            c = m.get("content")
            if isinstance(c, list):
                c = " ".join(p.get("text", "") for p in c if isinstance(p, dict))
            return c or ""
    return ""

def plan(body):
    msgs = body.get("messages", [])
    user = last_user(msgs).lower()
    last = msgs[-1] if msgs else {}
    tools = body.get("tools") or []
    names = [t.get("function", {}).get("name") for t in tools]
    if last.get("role") == "tool":
        return {"text": "The tool finished. Its output was:\n\n```\n" + str(last.get("content"))[:300] + "\n```\n\nDone."}
    if "usetool" in user and "terminal" in names:
        return {"tool": ("terminal", {"command": "echo hello-from-tool && sleep 1 && echo second-line"})}
    if "usedanger" in user and "terminal" in names:
        return {"tool": ("terminal", {"command": "rm -rf @ROOT@/work/victim"})}
    if "usefile" in user and "write_file" in names:
        return {"tool": ("write_file", {"path": "@ROOT@/work/note.txt", "content": "line one\nline two\nline three\n"})}
    if "usehacallobj" in user and "tool_call" in names:
        return {"tool": ("tool_call", {"calls": [{"name": "ha_call_service", "arguments": {"domain": "light", "service": "turn_on", "entity_id": "light.kitchen", "data": {"brightness": 120}}}]})}
    if "usehacall" in user and "tool_call" in names:
        return {"tool": ("tool_call", {"calls": [{"name": "ha_call_service", "arguments": {"domain": "light", "service": "turn_on", "entity_id": "light.kitchen", "data": "{\"brightness\": 120}"}}]})}
    if "useha" in user and "tool_call" in names:
        return {"tool": ("tool_call", {"calls": [{"name": "ha_list_entities", "arguments": {"domain": "light"}}, {"name": "ha_get_state", "arguments": {"entity_id": "light.kitchen"}}, {"name": "ha_list_services", "arguments": {"domain": "light"}}]})}
    if "markdown" in user:
        return {"text": MARKDOWN}
    if "long" in user:
        return {"text": "\n\n".join(f"Paragraph {i}: " + "lorem ipsum dolor sit amet " * 12 for i in range(1, 31))}
    if "think" in user:
        return {"reasoning": "Let me reason about this step by step. First consider A. Then consider B. Finally conclude C.", "text": "After thinking, the answer is 42."}
    return {"text": "Hello from the scripted model."}

class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self, *a):
        pass
    def _json(self, obj, code=200):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            return self._json({"object": "list", "data": [{"id": MODEL, "object": "model", "owned_by": "fake", "context_length": 128000}]})
        self._json({"ok": True})
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        if LOG:
            LOG.write(json.dumps({"t": time.time(), "path": self.path, "stream": body.get("stream"), "n_messages": len(body.get("messages", [])), "n_tools": len(body.get("tools") or []), "tool_names": sorted(t.get("function", {}).get("name", "") for t in (body.get("tools") or [])), "system_len": len(str(body.get("messages", [{}])[0].get("content", ""))) if body.get("messages") else 0, "last_user": last_user(body.get("messages", []))[:200], "keys": sorted(body.keys())}) + "\n")
            LOG.flush()
        try:
            tag = open("@ROOT@/dump_tag").read().strip()
        except OSError:
            tag = ""
        if tag:
            import os
            os.makedirs("@ROOT@/dumps", exist_ok=True)
            DUMPN[0] += 1
            with open("@ROOT@/dumps/%s_%03d.json" % (tag, DUMPN[0]), "w") as f:
                json.dump({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()}, "body": body}, f, indent=1, sort_keys=True, ensure_ascii=False)
        p = plan(body)
        usage = {"prompt_tokens": 1200, "completion_tokens": 80, "total_tokens": 1280}
        cid = "chatcmpl-fake%d" % int(time.time() * 1000)
        if not body.get("stream"):
            msg = {"role": "assistant", "content": p.get("text")}
            if "tool" in p:
                msg = {"role": "assistant", "content": None, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": p["tool"][0], "arguments": json.dumps(p["tool"][1])}}]}
            return self._json({"id": cid, "object": "chat.completion", "created": int(time.time()), "model": MODEL, "choices": [{"index": 0, "message": msg, "finish_reason": "tool_calls" if "tool" in p else "stop"}], "usage": usage})
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        def emit(delta, finish=None, extra=None):
            obj = {"id": cid, "object": "chat.completion.chunk", "created": int(time.time()), "model": MODEL, "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            if extra:
                obj.update(extra)
            self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
            self.wfile.flush()
        emit({"role": "assistant", "content": ""})
        if "reasoning" in p:
            for w in p["reasoning"].split(" "):
                emit({"reasoning_content": w + " "})
                time.sleep(0.02)
        if "tool" in p:
            name, args = p["tool"]
            emit({"tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": name, "arguments": ""}}]})
            s = json.dumps(args)
            for i in range(0, len(s), 12):
                emit({"tool_calls": [{"index": 0, "function": {"arguments": s[i:i+12]}}]})
                time.sleep(0.01)
            emit({}, "tool_calls")
        else:
            text = p["text"]
            step = 14
            for i in range(0, len(text), step):
                emit({"content": text[i:i+step]})
                time.sleep(0.012)
            emit({}, "stop")
        self.wfile.write(b"data: " + json.dumps({"id": cid, "object": "chat.completion.chunk", "created": int(time.time()), "model": MODEL, "choices": [], "usage": usage}).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

srv = ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H)
srv.daemon_threads = True
srv.serve_forever()
