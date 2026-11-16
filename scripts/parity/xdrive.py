import sys, os, pty, json, time, select, base64, subprocess, fcntl, termios, struct, signal
spec = json.load(open(sys.argv[1])); outdir = sys.argv[2]; os.makedirs(outdir, exist_ok=True)
rows, cols = spec["rows"], spec["cols"]
br = subprocess.Popen(["node", "@ROOT@/xterm/bridge.js"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, cwd="@ROOT@/xterm")
def tx(o): br.stdin.write((json.dumps(o) + "\n").encode()); br.stdin.flush()
def rx(): return json.loads(br.stdout.readline())
tx({"op": "open", "cols": cols, "rows": rows}); rx()
pid, fd = pty.fork()
if pid == 0:
    env = dict(os.environ, TERM="xterm-256color", COLORTERM="truecolor", TERM_PROGRAM="vscode", **spec.get("env", {}))
    os.chdir(spec.get("cwd", ".")); os.execvpe(spec["argv"][0], spec["argv"], env)
def setsize(r, c): fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", r, c, 0, 0))
setsize(rows, cols); raw = open(outdir + "/raw.bin", "wb"); t0 = time.time(); total = 0; wid = 0
def feed(data):
    global wid
    wid += 1; tx({"op": "write", "data": base64.b64encode(data).decode(), "id": wid})
    while True:
        m = rx()
        if "reply" in m: os.write(fd, base64.b64decode(m["reply"]))
        elif m.get("ok") == "write" and m.get("id") == wid: return
def pump(timeout):
    global total
    got = 0; end = time.time() + timeout
    while True:
        left = end - time.time()
        if left <= 0: return got
        r, _, _ = select.select([fd], [], [], left)
        if not r: return got
        try: data = os.read(fd, 65536)
        except OSError: return got
        if not data: return got
        raw.write(data); raw.flush(); total += len(data); got += len(data); feed(data)
for st in spec["steps"]:
    k = st[0]
    if k == "quiet":
        end = time.time() + st[2]
        while time.time() < end:
            if pump(st[1]) == 0: break
    elif k == "sleep": pump(st[1])
    elif k == "send": os.write(fd, st[1].encode()); pump(0.05)
    elif k == "type":
        for ch in st[1]: os.write(fd, ch.encode()); pump(0.01)
    elif k == "resize": setsize(st[1], st[2]); tx({"op": "resize", "cols": st[2], "rows": st[1]}); rx(); os.kill(pid, signal.SIGWINCH); pump(0.2)
    elif k == "snap":
        tx({"op": "snap", "name": st[1]}); m = rx()
        while "snap" not in m:
            if "reply" in m: os.write(fd, base64.b64decode(m["reply"]))
            m = rx()
        open(f"{outdir}/{st[1]}.txt", "w").write("\n".join(m["lines"]) + "\n")
        open(f"{outdir}/{st[1]}.all.txt", "w").write("\n".join(m["all"]) + "\n")
        print(json.dumps({"snap": st[1], "at": round(time.time() - t0, 3), "bytes": total, "cursor": m["cursor"], "base": m["base"]}), flush=True)
try: os.kill(pid, signal.SIGKILL)
except Exception: pass
br.kill()
