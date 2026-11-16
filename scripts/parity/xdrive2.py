import sys, os, pty, json, time, select, base64, subprocess, fcntl, termios, struct, signal, re
# Drives a program under headless xterm.js with wait-for-regex steps, style snapshots and a write timeline
spec = json.load(open(sys.argv[1])); outdir = sys.argv[2]; os.makedirs(outdir, exist_ok=True)
rows, cols = spec["rows"], spec["cols"]
br = subprocess.Popen(["node", "@ROOT@/xterm/bridge2.js"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, cwd="@ROOT@/xterm")
def tx(o): br.stdin.write((json.dumps(o) + "\n").encode()); br.stdin.flush()
def rx(): return json.loads(br.stdout.readline())
tx({"op": "open", "cols": cols, "rows": rows}); rx()
pid, fd = pty.fork()
if pid == 0:
    env = dict(os.environ, TERM="xterm-256color", COLORTERM="truecolor", TERM_PROGRAM="vscode", **spec.get("env", {}))
    env.pop("NO_COLOR", None)
    os.chdir(spec.get("cwd", ".")); os.execvpe(spec["argv"][0], spec["argv"], env)
def setsize(r, c): fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", r, c, 0, 0))
setsize(rows, cols); raw = open(outdir + "/raw.bin", "wb"); t0 = time.time(); total = 0; wid = 0; writes = 0
timeline = open(outdir + "/timeline.jsonl", "w"); alive = True; report = []
def feed(data):
    global wid
    wid += 1; tx({"op": "write", "data": base64.b64encode(data).decode(), "id": wid})
    while True:
        m = rx()
        if "reply" in m: os.write(fd, base64.b64decode(m["reply"]))
        elif m.get("ok") == "write" and m.get("id") == wid: return
def pump(timeout):
    global total, writes, alive
    got = 0; end = time.time() + timeout
    while True:
        left = end - time.time()
        if left <= 0: return got
        r, _, _ = select.select([fd], [], [], left)
        if not r: return got
        try: data = os.read(fd, 65536)
        except OSError: alive = False; return got
        if not data: alive = False; return got
        raw.write(data); raw.flush(); total += len(data); got += len(data); writes += 1
        timeline.write(json.dumps({"t": round(time.time() - t0, 4), "n": len(data)}) + "\n"); feed(data)
def snap(name, styles=False):
    tx({"op": "snap", "name": name, "styles": styles}); m = rx()
    while "snap" not in m:
        if "reply" in m: os.write(fd, base64.b64decode(m["reply"]))
        m = rx()
    return m
for st in spec["steps"]:
    k = st[0]; started = time.time()
    if not alive and k != "snap": continue
    if k == "quiet":
        end = time.time() + st[2]
        while time.time() < end and alive:
            if pump(st[1]) == 0: break
    elif k == "wait":
        pat = re.compile(st[1], re.S); ok = False
        while time.time() - started < st[2] and alive:
            pump(0.05)
            if pat.search("\n".join(snap("_")["lines"])): ok = True; break
        if len(st) > 3: pump(st[3])
        report.append({"wait": st[1], "ok": ok, "secs": round(time.time() - started, 3), "at": round(time.time() - t0, 3), "bytes": total})
    elif k == "waitgone":
        pat = re.compile(st[1], re.S); ok = False
        while time.time() - started < st[2] and alive:
            pump(0.05)
            if not pat.search("\n".join(snap("_")["lines"])): ok = True; break
        if len(st) > 3: pump(st[3])
        report.append({"waitgone": st[1], "ok": ok, "secs": round(time.time() - started, 3), "at": round(time.time() - t0, 3), "bytes": total})
    elif k == "sleep": pump(st[1])
    elif k == "send": os.write(fd, st[1].encode()); pump(0.05)
    elif k == "type":
        for ch in st[1]: os.write(fd, ch.encode()); pump(st[2] if len(st) > 2 else 0.01)
    elif k == "resize": setsize(st[1], st[2]); tx({"op": "resize", "cols": st[2], "rows": st[1]}); rx(); os.kill(pid, signal.SIGWINCH); pump(0.2)
    elif k == "mark":
        report.append({"mark": st[1], "at": round(time.time() - t0, 3), "bytes": total, "writes": writes})
    elif k == "snap":
        m = snap(st[1], True)
        open(f"{outdir}/{st[1]}.txt", "w").write("\n".join(m["lines"]) + "\n")
        open(f"{outdir}/{st[1]}.all.txt", "w").write("\n".join(m["all"]) + "\n")
        json.dump(m.get("styles", []), open(f"{outdir}/{st[1]}.attrs.json", "w"))
        report.append({"snap": st[1], "at": round(time.time() - t0, 3), "bytes": total, "writes": writes, "cursor": m["cursor"], "base": m["base"], "alive": alive})
try:
    os.kill(pid, signal.SIGTERM); time.sleep(0.3); os.kill(pid, signal.SIGKILL)
except Exception: pass
try: os.waitpid(pid, 0)
except Exception: pass
br.kill()
json.dump({"report": report, "bytes": total, "writes": writes, "alive_at_end": alive}, open(outdir + "/report.json", "w"), indent=1)
for r in report: print(json.dumps(r))
