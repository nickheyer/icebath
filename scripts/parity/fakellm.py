import json, sys, time, threading, re, socket, struct
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
    if "usewebsearch" in user and "web_search" in names:
        return {"tool": ("web_search", {"query": "parity probe", "limit": 3})}
    if "usewebextract" in user and "web_extract" in names:
        return {"tool": ("web_extract", {"urls": ["https://example.com/"]})}
    if "usehaarea" in user and "tool_call" in names:
        return {"tool": ("tool_call", {"calls": [{"name": "ha_list_entities", "arguments": {"area": "Kitchen"}}]})}
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

# Scripted agent loop cases, picked by a scn_<name> token in a user message and stepped per dump tag
SCN_RE = re.compile(r"\bscn_([a-z0-9]+)\b")
SCN_COUNTS = {}
SCN_LOCK = threading.Lock()
def E(msg, typ, code=None, param=None):
    return {"error": {"message": msg, "type": typ, "param": param, "code": code}}
OK = {"content": "Recovered reply from the scripted model."}
RL = E("Rate limit reached for fake-model on requests per min (RPM): Limit 3, Used 3, Requested 1.", "requests", "rate_limit_exceeded")
SCENARIOS = {
    "r429": [{"status": 429, "body": RL}, OK],
    "r429ra": [{"status": 429, "headers": {"retry-after": "3"}, "body": RL}, OK],
    "r429x": [{"status": 429, "body": RL}],
    "r429rax": [{"status": 429, "headers": {"retry-after": "2"}, "body": RL}],
    "r500": [{"status": 500, "body": E("The server had an error while processing your request. Sorry about that!", "server_error")}, OK],
    "r500x": [{"status": 500, "body": E("The server had an error while processing your request. Sorry about that!", "server_error")}],
    "r502": [{"status": 502, "ctype": "text/html", "raw": "<html><head><title>502 Bad Gateway</title></head><body><center><h1>502 Bad Gateway</h1></center></body></html>\n"}, OK],
    "r503": [{"status": 503, "body": E("The engine is currently overloaded, please try again later", "server_error")}, OK],
    "r503ra": [{"status": 503, "headers": {"retry-after": "2"}, "body": E("The engine is currently overloaded, please try again later", "server_error")}, OK],
    "r529": [{"status": 529, "body": {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}}, OK],
    "r529x": [{"status": 529, "body": {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}}],
    "b400": [{"status": 400, "body": E("Invalid value for 'temperature': expected a number.", "invalid_request_error", None, "temperature")}, OK],
    "b400ctx": [{"status": 400, "body": E("This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens. Please reduce the length of the messages.", "invalid_request_error", "context_length_exceeded", "messages")}, OK],
    "b400model": [{"status": 400, "body": E("The model `fake-model` does not exist", "invalid_request_error", "model_not_found", "model")}, OK],
    "b400args": [{"status": 400, "body": E("invalid tool call arguments", "invalid_request_error")}, OK],
    "b400policy": [{"status": 400, "body": E("Your request was flagged by our safety system as potentially violating our usage policy.", "invalid_request_error", "content_filter")}, OK],
    "b400role": [{"status": 400, "body": E("Conversation roles must alternate user/assistant/user/assistant/...", "invalid_request_error")}, OK],
    "b400param": [{"status": 400, "body": E("Unsupported parameter: 'reasoning_effort' is not supported with this model.", "invalid_request_error", "unsupported_parameter", "reasoning_effort")}, OK],
    "b400empty": [{"status": 400, "body": E("messages must have non-empty content", "invalid_request_error")}, OK],
    "b402": [{"status": 402, "body": E("Insufficient credits. Add more using https://example.com/credits", "billing", "insufficient_credits")}, OK],
    "b429quota": [{"status": 429, "body": E("You exceeded your current quota, please check your plan and billing details.", "insufficient_quota", "insufficient_quota")}, OK],
    "b401": [{"status": 401, "body": E("Incorrect API key provided: sk-fake-***rity. You can find your API key at https://platform.openai.com/account/api-keys.", "invalid_request_error", "invalid_api_key")}, OK],
    "b403": [{"status": 403, "body": E("You are not allowed to sample from this model", "permission_error")}, OK],
    "b403waf": [{"status": 403, "ctype": "text/html", "raw": "<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head><body><h1>Sorry, you have been blocked</h1></body></html>\n"}, OK],
    "b404": [{"status": 404, "body": E("The model `fake-model` does not exist or you do not have access to it.", "invalid_request_error", "model_not_found")}, OK],
    "b413": [{"status": 413, "body": E("Request entity too large", "invalid_request_error")}, OK],
    "b422": [{"status": 422, "body": {"detail": [{"loc": ["body", "messages"], "msg": "field required", "type": "value_error.missing"}]}}, OK],
    "reset": [{"content": "Partial answer that the connection cuts short before it can finish.", "cut_after": 2}, OK],
    "resetx": [{"content": "Partial answer that the connection cuts short before it can finish.", "cut_after": 2}],
    "early": [{"content": "Partial text with no finish reason.", "early_end": True}, OK],
    "earlyx": [{"content": "Partial text with no finish reason.", "early_end": True}],
    "empty": [{"content": ""}, OK],
    "emptyx": [{"content": ""}],
    "nochoices": [{"nochoices": True}, OK],
    "nochoicesx": [{"nochoices": True}],
    "badjson": [{"bad_sse": True}, OK],
    "badjsonx": [{"bad_sse": True}],
    "badargs": [{"tools": [["terminal", "{\"command\": \"echo hi\""]]}, OK],
    "badargsx": [{"tools": [["terminal", "{\"command\": \"echo hi\""]]}],
    "stall": [{"content": "Start of a reply that then stalls.", "stall": 40}, OK],
    "stallx": [{"content": "Start of a reply that then stalls.", "stall": 40}],
    "stall0": [{"content": "Reply after a silent stall.", "stall_before": 40}, OK],
    "stall0x": [{"content": "Reply after a silent stall.", "stall_before": 40}],
    "hang": [{"sleep": 40}, OK],
    "hangx": [{"sleep": 40}],
    "fb429": [{"status": 429, "body": RL}],
    "fb429@fb-model": [{"content": "Answer from the fallback model."}],
    "fb500": [{"status": 500, "body": E("The server had an error while processing your request. Sorry about that!", "server_error")}],
    "fb500@fb-model": [{"content": "Answer from the fallback model."}],
    "fb529": [{"status": 529, "body": {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}}],
    "fb529@fb-model": [{"content": "Answer from the fallback model."}],
    "fb401": [{"status": 401, "body": E("Incorrect API key provided: sk-fake-***rity.", "invalid_request_error", "invalid_api_key")}, OK],
    "fb401@fb-model": [{"content": "Answer from the fallback model."}],
    "fb400": [{"status": 400, "body": E("Invalid value for 'temperature': expected a number.", "invalid_request_error", None, "temperature")}],
    "fb400@fb-model": [{"content": "Answer from the fallback model."}],
    "fbargs": [{"status": 400, "body": E("invalid tool call arguments", "invalid_request_error")}],
    "fbargs@fb-model": [{"content": "Answer from the fallback model."}],
    "fbchain": [{"status": 429, "body": RL}],
    "fbchain@fb-model": [{"status": 429, "body": RL}],
    "fbchain@fb2-model": [{"content": "Answer from the second fallback model."}],
    "fbrl": [{"status": 429, "body": RL}, OK],
    "fbrl@fb-model": [{"content": "Answer from the fallback model."}, {"content": "Second answer from the fallback model."}],
    "fbempty": [{"content": ""}],
    "fbempty@fb-model": [{"content": "Answer from the fallback model."}],
}

def scn_name(body):
    for m in body.get("messages", []):
        if m.get("role") == "user":
            c = m.get("content")
            if isinstance(c, list):
                c = " ".join(p.get("text", "") for p in c if isinstance(p, dict))
            hit = SCN_RE.search(c or "")
            if hit and hit.group(1) in SCENARIOS:
                return hit.group(1)
    return None

def scn_step(body, tag):
    name = scn_name(body)
    if not name:
        return None, None, None
    kind = "main" if body.get("tools") else "aux"
    if (name + "@" + str(body.get("model"))) in SCENARIOS:
        name = name + "@" + str(body.get("model"))
    steps = SCENARIOS[name] if kind == "main" else SCENARIOS.get(name + "_aux")
    if not steps:
        return None, None, None
    with SCN_LOCK:
        key = (tag, name, kind)
        n = SCN_COUNTS.get(key, 0)
        SCN_COUNTS[key] = n + 1
    return name, n, steps[min(n, len(steps) - 1)]

def rst(sock):
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
    sock.close()

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
        if self.path == "/_scn_reset":
            with SCN_LOCK:
                SCN_COUNTS.clear()
            return self._json({"ok": True})
        if self.path.rstrip("/").endswith("/models"):
            return self._json({"object": "list", "data": [{"id": MODEL, "object": "model", "owned_by": "fake", "context_length": 128000}]})
        self._json({"ok": True})
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n)
        body = json.loads(raw or b"{}")
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
                json.dump({"path": self.path, "request_version": self.request_version, "header_lines": ["%s: %s" % kv for kv in self.headers.items()], "headers": {k.lower(): v for k, v in self.headers.items()}, "body": body}, f, indent=1, sort_keys=True, ensure_ascii=False)
            with open("@ROOT@/dumps/%s_%03d.raw" % (tag, DUMPN[0]), "wb") as f:
                f.write(raw)
        name, stepn, step = scn_step(body, tag)
        if step is not None:
            with open("@ROOT@/scn.log", "a") as f:
                f.write(json.dumps({"t": time.time(), "tag": tag, "scn": name, "step": stepn, "stream": bool(body.get("stream")), "n_messages": len(body.get("messages", []))}) + "\n")
            return self._scripted(body, step)
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
    def _scripted(self, body, step):
        if step.get("sleep"):
            time.sleep(step["sleep"])
        if "status" in step:
            data = step["raw"].encode() if "raw" in step else json.dumps(step["body"]).encode()
            self.send_response(step["status"])
            self.send_header("Content-Type", step.get("ctype", "application/json"))
            for k, v in step.get("headers", {}).items():
                self.send_header(k, v)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        usage = step.get("usage", {"prompt_tokens": 1200, "completion_tokens": 80, "total_tokens": 1280})
        cid = "chatcmpl-scn%d" % int(time.time() * 1000)
        tools = step.get("tools") or []
        text = step.get("content")
        finish = step.get("finish") or ("tool_calls" if tools else "stop")
        calls = [{"id": "call_%d" % (i + 1), "type": "function", "function": {"name": t[0], "arguments": t[1] if isinstance(t[1], str) else json.dumps(t[1])}} for i, t in enumerate(tools)]
        rkey = step.get("reasoning_key", "reasoning_content")
        if not body.get("stream"):
            if step.get("nochoices"):
                return self._json({"id": cid, "object": "chat.completion", "created": int(time.time()), "model": MODEL, "choices": [], "usage": usage})
            if step.get("bad_sse"):
                data = b'{"id": "' + cid.encode() + b'", "object": "chat.completion", "choices": [{"index": 0, "message": {"role": "assist'
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return
            msg = {"role": "assistant", "content": text}
            if calls:
                msg["tool_calls"] = calls
            if step.get("reasoning"):
                msg[rkey] = step["reasoning"]
            if step.get("cut_after"):
                data = json.dumps({"id": cid, "object": "chat.completion", "created": int(time.time()), "model": MODEL, "choices": [{"index": 0, "message": msg, "finish_reason": finish}], "usage": usage}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data[:len(data) // 2])
                self.wfile.flush()
                time.sleep(0.05)
                return rst(self.connection)
            return self._json({"id": cid, "object": "chat.completion", "created": int(time.time()), "model": MODEL, "choices": [{"index": 0, "message": msg, "finish_reason": finish}], "usage": usage})
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        def emit(delta, fin=None, choices=None, extra=None):
            obj = {"id": cid, "object": "chat.completion.chunk", "created": int(time.time()), "model": MODEL, "choices": [{"index": 0, "delta": delta, "finish_reason": fin}] if choices is None else choices}
            if extra:
                obj.update(extra)
            self.wfile.write(b"data: " + json.dumps(obj).encode() + b"\n\n")
            self.wfile.flush()
        emit({"role": "assistant", "content": ""})
        if step.get("nochoices"):
            emit(None, choices=[], extra={"usage": usage})
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return
        if step.get("bad_sse"):
            self.wfile.write(b'data: {"id": "' + cid.encode() + b'", "choices": [{"index": 0, "delta": {"content": "oops\n\n')
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return
        if step.get("stall_before"):
            time.sleep(step["stall_before"])
        if step.get("reasoning"):
            for w in step["reasoning"].split(" "):
                emit({rkey: w + " "})
                time.sleep(0.01)
        sent = 0
        for i in range(0, len(text or ""), 14):
            emit({"content": text[i:i+14]})
            sent += 1
            if step.get("cut_after") and sent >= step["cut_after"]:
                time.sleep(0.05)
                return rst(self.connection)
            if step.get("stall") and sent == 1:
                time.sleep(step["stall"])
            time.sleep(0.01)
        for ci, c in enumerate(calls):
            emit({"tool_calls": [{"index": ci, "id": c["id"], "type": "function", "function": {"name": c["function"]["name"], "arguments": ""}}]})
            s = c["function"]["arguments"]
            for i in range(0, len(s), 12):
                emit({"tool_calls": [{"index": ci, "function": {"arguments": s[i:i+12]}}]})
                time.sleep(0.005)
        if step.get("early_end"):
            return
        emit({}, finish)
        emit(None, choices=[], extra={"usage": usage})
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

srv =ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H)
srv.daemon_threads = True
srv.serve_forever()
