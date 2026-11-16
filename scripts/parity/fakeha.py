# Fake Home Assistant REST and WebSocket server, logs every request and frame to cap/fakeha_<tag>.log with tag read from the ha_tag file
import base64, hashlib, json, os, struct, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
L = "@ROOT@"
STATES = [
    {"entity_id": "light.kitchen", "state": "on", "attributes": {"friendly_name": "Kitchen Light", "brightness": 200}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
    {"entity_id": "light.porch", "state": "off", "attributes": {"friendly_name": "Porch Light"}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
    {"entity_id": "sensor.temp", "state": "21.5", "attributes": {"friendly_name": "Temperature", "unit_of_measurement": "C"}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
]
SERVICES = [{"domain": "light", "services": {"turn_on": {"name": "Turn on", "description": "Turn on a light", "fields": {"brightness": {"description": "Brightness"}}}, "turn_off": {"name": "Turn off", "description": "Turn off a light", "fields": {}}}}]
REGISTRY = {
    "config/area_registry/list": [{"area_id": "kitchen", "name": "Kitchen"}],
    "config/device_registry/list": [{"id": "dev1", "name": "Kitchen Lamp", "name_by_user": None, "area_id": "kitchen"}],
    "config/entity_registry/list": [{"entity_id": "light.kitchen", "device_id": "dev1", "area_id": None}, {"entity_id": "light.porch", "device_id": None, "area_id": None}],
}
TOKEN = "fake-ha-token"
HA_VERSION = "2026.11.0"
SUBS = []
SUBS_LOCK = threading.Lock()
LOG_LOCK = threading.Lock()

def tag():
    return open(L + "/ha_tag").read().strip() if os.path.exists(L + "/ha_tag") else "none"

def log(entry):
    with LOG_LOCK, open(L + "/cap/fakeha_%s.log" % tag(), "a") as f:
        f.write(json.dumps(entry, sort_keys=True) + "\n")

class WS:
    def __init__(self, rfile, wfile):
        self.r, self.w, self.lock = rfile, wfile, threading.Lock()
    def send(self, obj, opcode=1):
        data = json.dumps(obj).encode() if opcode == 1 else obj
        head = bytes([0x80 | opcode])
        n = len(data)
        head += bytes([n]) if n < 126 else (bytes([126]) + struct.pack("!H", n) if n < 65536 else bytes([127]) + struct.pack("!Q", n))
        with self.lock:
            self.w.write(head + data); self.w.flush()
    def recv(self):
        b = self.r.read(2)
        if len(b) < 2:
            return None, None
        opcode, n = b[0] & 0x0F, b[1] & 0x7F
        if n == 126: n = struct.unpack("!H", self.r.read(2))[0]
        elif n == 127: n = struct.unpack("!Q", self.r.read(8))[0]
        mask = self.r.read(4) if b[1] & 0x80 else b"\0\0\0\0"
        raw = self.r.read(n)
        return opcode, bytes(c ^ mask[i % 4] for i, c in enumerate(raw))

def broadcast(event):
    with SUBS_LOCK:
        subs = list(SUBS)
    for ws, sid in subs:
        try:
            ws.send({"id": sid, "type": "event", "event": event})
        except OSError:
            pass
    return len(subs)

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _log(self, body):
        log({"method": self.command, "path": self.path, "headers": [[k, v] for k, v in self.headers.items()], "body": body, "version": self.request_version})
    def _send(self, obj, code=200):
        raw = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
    def _websocket(self):
        key = self.headers.get("Sec-WebSocket-Key", "")
        accept = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        self.send_response(101); self.send_header("Upgrade", "websocket"); self.send_header("Connection", "Upgrade"); self.send_header("Sec-WebSocket-Accept", accept); self.end_headers()
        ws = WS(self.rfile, self.wfile)
        ws.send({"type": "auth_required", "ha_version": HA_VERSION})
        authed = False
        try:
            while True:
                opcode, data = ws.recv()
                if opcode is None or opcode == 8:
                    log({"ws": "close", "path": self.path})
                    return
                if opcode == 9:
                    ws.send(data, 10); continue
                if opcode != 1:
                    continue
                msg = json.loads(data.decode())
                log({"ws": "recv", "path": self.path, "data": msg})
                if msg.get("type") == "auth":
                    authed = msg.get("access_token") == TOKEN
                    ws.send({"type": "auth_ok", "ha_version": HA_VERSION} if authed else {"type": "auth_invalid", "message": "Invalid access token or password"})
                    if not authed: return
                elif msg.get("type") == "subscribe_events" and authed:
                    with SUBS_LOCK: SUBS.append((ws, msg["id"]))
                    ws.send({"id": msg["id"], "type": "result", "success": True, "result": None})
                elif msg.get("type") in REGISTRY and authed:
                    ws.send({"id": msg["id"], "type": "result", "success": True, "result": REGISTRY[msg["type"]]})
                elif authed:
                    ws.send({"id": msg.get("id"), "type": "result", "success": False, "error": {"code": "unknown_command", "message": "Unknown command."}})
        finally:
            with SUBS_LOCK:
                SUBS[:] = [s for s in SUBS if s[0] is not ws]
    def do_GET(self):
        self._log("")
        if self.path == "/api/websocket" and self.headers.get("Upgrade", "").lower() == "websocket": return self._websocket()
        if self.path == "/api/states": return self._send(STATES)
        if self.path.startswith("/api/states/"):
            for s in STATES:
                if s["entity_id"] == self.path.rsplit("/", 1)[1]: return self._send(s)
            return self._send({"message": "Entity not found."}, 404)
        if self.path == "/api/services": return self._send(SERVICES)
        return self._send({"message": "Not found"}, 404)
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
        if self.path == "/_fake/event":
            return self._send({"delivered": broadcast(json.loads(body))})
        self._log(body)
        if self.path.startswith("/api/services/"): return self._send([{"entity_id": "light.kitchen", "state": "on", "attributes": {"friendly_name": "Kitchen Light"}}])
        return self._send({"message": "Not found"}, 404)
srv = ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H)
srv.daemon_threads = True
srv.serve_forever()
