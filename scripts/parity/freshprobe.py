import subprocess, sys, os, re, shutil, time, signal
ROOT = "@ROOT@"
FRESH = ROOT + "/fresh"
def reap(marker):
    for pid in os.listdir("/proc"):
        if not pid.isdigit() or int(pid) == os.getpid():
            continue
        try:
            env = open(f"/proc/{pid}/environ", "rb").read()
        except OSError:
            continue
        if marker.encode() in env:
            try:
                os.kill(int(pid), signal.SIGKILL)
            except OSError:
                pass
def tree(base, depth=4):
    out = []
    for d, dirs, files in os.walk(base):
        rel = os.path.relpath(d, base)
        if rel != "." and rel.count(os.sep) >= depth - 1:
            dirs[:] = []
        for n in files:
            out.append(os.path.normpath(os.path.join(rel, n)))
        for n in dirs:
            out.append(os.path.normpath(os.path.join(rel, n)) + "/")
    return sorted(out)
def norm(s):
    s = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", s)
    s = s.replace("HERMES", "ICEBATH").replace("Hermes", "Icebath").replace("hermes-agent", "icebath").replace("hermes", "icebath")
    return s.replace(FRESH + "/ref-home", "<HOME>").replace(FRESH + "/ib-home", "<HOME>")
def run(which, cmds, keep):
    home, uh = f"{FRESH}/{which}-home", f"{FRESH}/uh-{which}"
    if not keep:
        reap(home)
        shutil.rmtree(home, ignore_errors=True); shutil.rmtree(uh, ignore_errors=True)
        os.makedirs(home); os.makedirs(uh)
        for f in ("config.yaml", ".env"):
            shutil.copy(f"{ROOT}/{which}-home/{f}", f"{home}/{f}")
    env = dict(os.environ, HOME=uh, NO_COLOR="1", COLUMNS="120", LINES="40", TERM="xterm-256color")
    env["HERMES_HOME" if which == "ref" else "ICEBATH_HOME"] = home
    res = []
    for cmd in cmds:
        t = time.time()
        try:
            p = subprocess.run([f"{ROOT}/{which}"] + cmd.split(), env=env, cwd=f"{ROOT}/work", stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=120)
            rc, o, e = p.returncode, p.stdout, p.stderr
        except subprocess.TimeoutExpired:
            rc, o, e = "TO", "", ""
        res.append((cmd, rc, norm(o), norm(e), time.time() - t))
    return res, set(tree(home)), set(tree(uh))
keep = "--keep" in sys.argv
cmds = [a for a in sys.argv[1:] if a != "--keep"]
r, rt, ru = run("ref", cmds, keep)
b, bt, bu = run("ib", cmds, keep)
for (c, rrc, ro, re_, rdt), (_, brc, bo, be, bdt) in zip(r, b):
    print("== %s  rc %s/%s  t %.2f/%.2f  stdout %s  stderr %s" % (c, rrc, brc, rdt, bdt, "same" if ro == bo else "DIFF", "same" if re_ == be else "DIFF"))
    if re_ != be:
        print("  ref stderr: " + re_.strip()[:600].replace("\n", "\n              "))
        print("  ib  stderr: " + be.strip()[:600].replace("\n", "\n              "))
strip = lambda s: {re.sub(r"\d{5,}", "N", x) for x in s}
rt, bt = strip(rt), strip(bt)
print("home only ref:", " ".join(sorted(rt - bt))[:1500])
print("home only ib :", " ".join(sorted(bt - rt))[:1500])
print("userhome only ref:", " ".join(sorted(ru - bu))[:600])
print("userhome only ib :", " ".join(sorted(bu - ru))[:600])
if "--keep" not in sys.argv:
    time.sleep(0.2)
