import subprocess, sys, os, re
ROOT = "@ROOT@"
CMDS = [l.strip() for l in open(sys.argv[1]) if l.strip() and not l.startswith("#")]
def size(p):
    try:
        return os.path.getsize(p)
    except OSError:
        return 0
def count(p, off, needle):
    try:
        with open(p, "rb") as f:
            f.seek(off)
            return f.read().count(needle)
    except OSError:
        return 0
def run(which, cmd):
    home = f"{ROOT}/{which}-home/logs"
    a, e = size(home + "/agent.log"), size(home + "/errors.log")
    env = dict(os.environ, HOME=f"{ROOT}/uh-{which}", NO_COLOR="1", COLUMNS="120", LINES="40", TERM="xterm-256color")
    try:
        subprocess.run([f"{ROOT}/{which}"] + cmd.split(), env=env, cwd=f"{ROOT}/work", stdin=subprocess.DEVNULL, capture_output=True, timeout=60)
    except subprocess.TimeoutExpired:
        pass
    return size(home + "/agent.log") - a, size(home + "/errors.log") - e, count(home + "/agent.log", a, b"Plugin discovery complete")
for c in CMDS:
    r = run("ref", c); b = run("ib", c)
    flag = "" if (r[2] == b[2] and abs(r[0] - b[0]) < 400 and r[1] == b[1]) else "  <<<"
    print("%-40s agent %6d/%6d errors %5d/%5d discovery %d/%d%s" % (c[:40], r[0], b[0], r[1], b[1], r[2], b[2], flag), flush=True)
