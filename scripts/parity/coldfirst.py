#!/usr/bin/env python3
# Measures the first --tui reply on a brand new home with a cold gateway for one side
import json, os, shutil, signal, subprocess, sys, time

lane, side, tag = sys.argv[1], sys.argv[2], sys.argv[3]
quit_after = float(sys.argv[4]) if len(sys.argv) > 4 else 0.0
home = f"{lane}/fresh/{side}-{tag}"
out = f"{lane}/cap/cold_{side}_{tag}"
shutil.rmtree(home, ignore_errors=True)
os.makedirs(home)
shutil.copy(f"{lane}/{side}-home/config.yaml", f"{home}/config.yaml")
var = "ICEBATH_HOME" if side == "ib" else "HERMES_HOME"
spec = {
    "argv": [f"{lane}/{side}", "--tui"],
    "env": {"HOME": f"{lane}/uh-{side}", var: home, "HERMES_TUI_DIR": f"{lane}/refsrc/ui-tui", "BROWSER": "/bin/true"},
    "rows": 40, "cols": 120, "cwd": f"{lane}/work",
    "steps": [["wait", "ready", 90, 0.3], ["snap", "01_start"], ["type", "hello there"], ["mark", "typed"], ["send", "\r"],
              ["wait", "Hello from the scripted model", 60, 0.0], ["mark", "reply"], ["sleep", 1.0], ["snap", "03_reply"],
              ["send", "\u0004"], ["sleep", 1.0]],
}
os.makedirs(out, exist_ok=True)
json.dump(spec, open(f"{out}/spec.json", "w"))
env = dict(os.environ)
for k in ("DISPLAY", "WAYLAND_DISPLAY"):
    env.pop(k, None)
proc = subprocess.Popen([f"{lane}/ref-venv/bin/python", f"{lane}/scripts/xdrive2.py", f"{out}/spec.json", out],
                        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

def gateway_pid():
    try:
        return json.load(open(f"{home}/gateway.pid"))["pid"]
    except (OSError, ValueError, KeyError):
        return 0

log = f"{home}/logs/agent.log"
dumped = False
while proc.poll() is None:
    if quit_after and not dumped and os.path.exists(log) and "inbound message" in open(log, errors="replace").read():
        time.sleep(quit_after)
        pid = gateway_pid()
        if pid:
            os.kill(pid, signal.SIGQUIT)
        dumped = True
    time.sleep(0.05)
rep = json.load(open(f"{out}/report.json"))["report"]
marks = {e.get("mark") or e.get("wait"): e["at"] for e in rep if "mark" in e or "wait" in e}
lines = [l for l in open(log, errors="replace") if any(k in l for k in ("Starting", "ws accepted", "inbound message", "conversation turn", "API call #1"))]
print(json.dumps({"side": side, "ready_s": marks.get("ready"), "typed_s": marks.get("typed"), "reply_s": marks.get("reply"),
                  "first_reply_latency_s": round(marks.get("reply", 0) - marks.get("typed", 0), 3),
                  "bytes": os.path.getsize(f"{out}/raw.bin"), "out": out}))
for l in lines[:8]:
    print(l[:150].rstrip())
pid = gateway_pid()
if pid and not dumped:
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
