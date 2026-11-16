#!/usr/bin/env python3
# Prints median wall, CPU, RSS and terminal output of the reference and icebath for cli, chat, tty and gateway rows
# usage: perfbench.py <lane-dir> <out.txt> [cli] [chat] [tty] [gateway]   env PERF_RUNS (default 5), PERF_IDLE (default 20)
import glob, json, os, signal, statistics, subprocess, sys, threading, time

lane, out = sys.argv[1], sys.argv[2]
groups = sys.argv[3:] or ["cli", "chat", "tty", "gateway"]
RUNS = int(os.environ.get("PERF_RUNS", "5"))
IDLE = float(os.environ.get("PERF_IDLE", "20"))
SIDES = ("ref", "ib")
HOMEVAR = {"ref": "HERMES_HOME", "ib": "ICEBATH_HOME"}
LOG = f"{lane}/fakellm.log"
TICK, PAGE = os.sysconf("SC_CLK_TCK"), os.sysconf("SC_PAGE_SIZE")
table = []


# Environment of one side with its isolated user home and no display
def env_for(side):
    e = dict(os.environ, HOME=f"{lane}/uh-{side}", NO_COLOR="1", COLUMNS="120", LINES="40", TERM="xterm-256color", BROWSER="/bin/true")
    e.pop("DISPLAY", None)
    e.pop("WAYLAND_DISPLAY", None)
    return e


# Runs one command to completion and returns wall seconds, CPU seconds and peak RSS in MB from wait4
def timed(side, args, timeout=180):
    t0w, t0 = time.time(), time.monotonic()
    p = subprocess.Popen([f"{lane}/{side}"] + args, env=env_for(side), cwd=f"{lane}/work", stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    guard = threading.Timer(timeout, lambda: os.kill(p.pid, signal.SIGKILL))
    guard.start()
    _, status, ru = os.wait4(p.pid, 0)
    guard.cancel()
    return {"wall": time.monotonic() - t0, "cpu": ru.ru_utime + ru.ru_stime, "rss": ru.ru_maxrss / 1024.0, "t0": t0w, "t1": time.time(),
            "rc": os.waitstatus_to_exitcode(status)}


# Scripted model requests logged at or after a wall clock instant
def requests_since(t):
    rows = []
    for line in open(LOG):
        try:
            r = json.loads(line)
        except ValueError:
            continue
        if r.get("t", 0) >= t:
            rows.append(r)
    return rows


# Every descendant pid of a process including itself
def tree(pid):
    pids, i = [pid], 0
    while i < len(pids):
        for path in glob.glob(f"/proc/{pids[i]}/task/*/children"):
            try:
                pids += [int(x) for x in open(path).read().split()]
            except OSError:
                pass
        i += 1
    return pids


# Summed CPU seconds and RSS in MB of a pid list read from /proc stat
def cpu_rss(pids):
    cpu = rss = 0.0
    for p in pids:
        try:
            s = open(f"/proc/{p}/stat").read()
            f = s[s.rindex(")") + 2:].split()
            cpu += (int(f[11]) + int(f[12])) / TICK
            rss += int(f[21]) * PAGE / 1048576.0
        except (OSError, ValueError):
            pass
    return cpu, rss


# Adds one table row from two sample lists and marks whether icebath is at least as good
def row(name, metric, unit, ref, ib):
    r, b = statistics.median(ref), statistics.median(ib)
    verdict = "ok" if b <= r else "SLOWER" if unit == "s" else "HEAVIER"
    table.append("%-22s %-26s %10.3f %10.3f %-3s %7.2fx  %s" % (name, metric, r, b, unit, (b / r) if r else float("inf") if b else 1.0, verdict))
    print(table[-1], flush=True)


# Wall, CPU and peak RSS of the non interactive commands
def cli():
    for cmd in (["--version"], ["--help"], ["status"], ["doctor"], ["sessions", "list"], ["tools", "list"]):
        res = {s: [] for s in SIDES}
        for s in SIDES:
            timed(s, cmd)
        for _ in range(RUNS):
            for s in SIDES:
                res[s].append(timed(s, cmd))
        name = " ".join(cmd)
        for metric, unit in (("wall", "s"), ("cpu", "s"), ("rss", "MB")):
            row(name, metric if metric != "rss" else "peak rss", unit, [x[metric] for x in res["ref"]], [x[metric] for x in res["ib"]])


# Single query chat timings against the scripted model request log
def chat():
    res = {(q, s): [] for q in ("hello", "long", "usetool") for s in SIDES}
    for q in ("hello", "long", "usetool"):
        for s in SIDES:
            timed(s, ["chat", "-q", q, "-Q"])
        for _ in range(RUNS):
            for s in SIDES:
                x = timed(s, ["chat", "-q", q, "-Q"])
                reqs = requests_since(x["t0"])
                agent = [r for r in reqs if r.get("n_tools")] or reqs
                if not agent:
                    raise SystemExit(f"{s} chat -q {q} made no model request, rc {x['rc']}")
                x["first"] = agent[0]["t"] - x["t0"]
                x["tail"] = x["t1"] - agent[0]["t"]
                x["trip"] = agent[1]["t"] - agent[0]["t"] if len(agent) > 1 else None
                x["nreq"] = len(reqs)
                res[(q, s)].append(x)
                time.sleep(0.3)
    pick = lambda q, k: ([x[k] for x in res[(q, "ref")]], [x[k] for x in res[(q, "ib")]])
    row("chat -q hello -Q", "wall", "s", *pick("hello", "wall"))
    row("chat -q hello -Q", "start to first request", "s", *pick("hello", "first"))
    row("chat -q hello -Q", "cpu", "s", *pick("hello", "cpu"))
    row("chat -q hello -Q", "peak rss", "MB", *pick("hello", "rss"))
    row("chat -q hello -Q", "model requests", "n", *pick("hello", "nreq"))
    row("chat -q long -Q", "first request to exit", "s", *pick("long", "tail"))
    row("chat -q long -Q", "peak rss", "MB", *pick("long", "rss"))
    if any(x["trip"] is None for s in SIDES for x in res[("usetool", s)]):
        raise SystemExit("chat -q usetool made a single agent request, the tool round trip did not happen")
    row("chat -q usetool -Q", "tool round trip", "s", *pick("usetool", "trip"))
    row("chat -q usetool -Q", "wall", "s", *pick("usetool", "wall"))


# One interactive classic session under xdrive.py, sampled from outside while it idles
def tty_once(side, n):
    outdir = f"{lane}/cap/perf_tty_{side}_{n}"
    os.makedirs(outdir, exist_ok=True)
    raw = f"{outdir}/raw.bin"
    if os.path.exists(raw):
        os.unlink(raw)
    spec = {"argv": [f"{lane}/{side}", "chat"], "env": {"HOME": f"{lane}/uh-{side}", "BROWSER": "/bin/true"}, "rows": 40, "cols": 120,
            "cwd": f"{lane}/work", "steps": [["quiet", 2.0, 40], ["snap", "ready"], ["sleep", IDLE], ["snap", "idle"]]}
    json.dump(spec, open(f"{outdir}/spec.json", "w"))
    env = dict(os.environ)
    env.pop("DISPLAY", None)
    env.pop("WAYLAND_DISPLAY", None)
    proc = subprocess.Popen([f"{lane}/ref-venv/bin/python", f"{lane}/scripts/xdrive.py", f"{outdir}/spec.json", outdir], env=env,
                            stdout=subprocess.PIPE, text=True)
    st = {"child": 0, "spawn": 0.0, "sizes": [], "samples": [], "stop": False}

    def poll():
        last, nxt = -1, 0.0
        while not st["stop"]:
            now = time.time()
            if not st["child"]:
                for c in tree(proc.pid)[1:]:
                    try:
                        if b"bridge.js" not in open(f"/proc/{c}/cmdline", "rb").read():
                            st["child"], st["spawn"] = c, now
                            break
                    except OSError:
                        pass
            try:
                size = os.stat(raw).st_size
            except OSError:
                size = 0
            if size != last:
                st["sizes"].append((now, size))
                last = size
            if st["child"] and now >= nxt:
                st["samples"].append((now,) + cpu_rss(tree(st["child"])))
                nxt = now + 0.2
            time.sleep(0.003)

    th = threading.Thread(target=poll, daemon=True)
    th.start()
    marks = {}
    for line in proc.stdout:
        try:
            m = json.loads(line)
        except ValueError:
            continue
        marks[m["snap"]] = (time.time(), m["bytes"], m["at"])
    proc.wait()
    st["stop"] = True
    th.join()
    if "ready" not in marks or "idle" not in marks:
        raise SystemExit(f"{side} interactive session produced no ready and idle snapshots, see {outdir}")
    data = open(raw, "rb").read()
    at = data.find("❯".encode())
    if at < 0:
        raise SystemExit(f"{side} interactive session never painted the prompt, see {outdir}")
    painted = next(t for t, size in st["sizes"] if size >= at + 3)
    if painted > marks["ready"][0]:
        raise SystemExit(f"{side} interactive session went quiet before the prompt was painted, see {outdir}")
    win = [x for x in st["samples"] if marks["ready"][0] <= x[0] <= marks["idle"][0] - 0.05]
    if len(win) < 2:
        raise SystemExit(f"{side} interactive session has no idle samples, see {outdir}")
    span = win[-1][0] - win[0][0]
    return {"prompt": painted - st["spawn"], "ready": marks["ready"][2], "cpu": (win[-1][1] - win[0][1]) / span * IDLE, "rss": win[-1][2],
            "bytes": float(marks["idle"][1] - marks["ready"][1])}


# Interactive classic session rows
def tty():
    res = {s: [] for s in SIDES}
    for n in range(RUNS):
        for s in SIDES:
            res[s].append(tty_once(s, n))
    pick = lambda k: ([x[k] for x in res["ref"]], [x[k] for x in res["ib"]])
    row("interactive classic", "start to painted prompt", "s", *pick("prompt"))
    row("interactive classic", "idle cpu per %gs" % IDLE, "s", *pick("cpu"))
    row("interactive classic", "tree rss while idle", "MB", *pick("rss"))
    row("interactive classic", "tty bytes per %gs idle" % IDLE, "B", *pick("bytes"))


# Pid recorded in a side's gateway.pid when that process is alive and belongs to this lane
def gateway_pid(side):
    try:
        pid = int(json.load(open(f"{lane}/{side}-home/gateway.pid"))["pid"])
        envb = open(f"/proc/{pid}/environ", "rb").read()
    except (OSError, ValueError, KeyError):
        return 0
    return pid if f"{HOMEVAR[side]}={lane}/{side}-home".encode() in envb else 0


# Terminates this lane's gateway of one side and waits until it is gone
def gateway_kill(side):
    pid = gateway_pid(side)
    if not pid:
        return
    os.kill(pid, signal.SIGTERM)
    end = time.time() + 30
    while time.time() < end and os.path.exists(f"/proc/{pid}"):
        time.sleep(0.05)
    if os.path.exists(f"/proc/{pid}"):
        os.kill(pid, signal.SIGKILL)
        time.sleep(0.5)


# Gateway autostart through `gateway ensure`, then idle CPU and RSS of the started gateway tree
def gateway():
    res = {s: [] for s in SIDES}
    for _ in range(RUNS):
        for s in SIDES:
            gateway_kill(s)
            x = timed(s, ["gateway", "ensure"])
            pid = gateway_pid(s)
            if x["rc"] != 0 or not pid:
                raise SystemExit(f"{s} gateway ensure rc {x['rc']} pid {pid}")
            time.sleep(1.0)
            c0, _ = cpu_rss(tree(pid))
            t0 = time.time()
            time.sleep(IDLE)
            c1, rss = cpu_rss(tree(pid))
            if not os.path.exists(f"/proc/{pid}"):
                raise SystemExit(f"{s} gateway {pid} exited inside the idle window")
            x["idle"] = (c1 - c0) / (time.time() - t0) * IDLE
            x["grss"] = rss
            res[s].append(x)
            gateway_kill(s)
    pick = lambda k: ([x[k] for x in res["ref"]], [x[k] for x in res["ib"]])
    row("gateway ensure", "autostart until ready", "s", *pick("wall"))
    row("gateway ensure", "ensure command peak rss", "MB", *pick("rss"))
    row("gateway run", "idle cpu per %gs" % IDLE, "s", *pick("idle"))
    row("gateway run", "tree rss while idle", "MB", *pick("grss"))


head = "%-22s %-26s %10s %10s %-3s %8s  %s" % ("row", "metric", "reference", "icebath", "u", "ib/ref", "verdict")
print(head, flush=True)
for g in groups:
    {"cli": cli, "chat": chat, "tty": tty, "gateway": gateway}[g]()
with open(out, "a") as f:
    f.write("# %s runs=%d idle=%g groups=%s binary=%d bytes\n" % (time.strftime("%Y-%m-%d %H:%M:%S"), RUNS, IDLE, ",".join(groups),
                                                                 os.path.getsize(f"{lane}/bin/icebath")))
    f.write(head + "\n" + "\n".join(table) + "\n")
