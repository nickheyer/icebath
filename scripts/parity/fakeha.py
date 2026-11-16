# Fake Home Assistant REST server, logs every request to cap/fakeha_<tag>.log with tag read from the ha_tag file
import json, sys, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
L = "@ROOT@"
STATES = [
    {"entity_id": "light.kitchen", "state": "on", "attributes": {"friendly_name": "Kitchen Light", "brightness": 200}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
    {"entity_id": "light.porch", "state": "off", "attributes": {"friendly_name": "Porch Light"}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
    {"entity_id": "sensor.temp", "state": "21.5", "attributes": {"friendly_name": "Temperature", "unit_of_measurement": "C"}, "last_changed": "2026-11-15T00:00:00+00:00", "last_updated": "2026-11-15T00:00:00+00:00"},
]
SERVICES = [{"domain": "light", "services": {"turn_on": {"name": "Turn on", "description": "Turn on a light", "fields": {"brightness": {"description": "Brightness"}}}, "turn_off": {"name": "Turn off", "description": "Turn off a light", "fields": {}}}}]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _log(self, body):
        tag = open(L + "/ha_tag").read().strip() if os.path.exists(L + "/ha_tag") else "none"
        hdr = [[k, v] for k, v in self.headers.items()]
        with open(L + "/cap/fakeha_%s.log" % tag, "a") as f:
            f.write(json.dumps({"method": self.command, "path": self.path, "headers": hdr, "body": body, "version": self.request_version}, sort_keys=True) + "\n")
    def _send(self, obj, code=200):
        raw = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
    def do_GET(self):
        self._log("")
        if self.path == "/api/states": return self._send(STATES)
        if self.path.startswith("/api/states/"):
            for s in STATES:
                if s["entity_id"] == self.path.rsplit("/", 1)[1]: return self._send(s)
            return self._send({"message": "Entity not found."}, 404)
        if self.path == "/api/services": return self._send(SERVICES)
        return self._send({"message": "Not found"}, 404)
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode()
        self._log(body)
        if self.path.startswith("/api/services/"): return self._send([{"entity_id": "light.kitchen", "state": "on", "attributes": {"friendly_name": "Kitchen Light"}}])
        return self._send({"message": "Not found"}, 404)
ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
