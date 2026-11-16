import subprocess, sys, time, re, os, difflib, json
OUT = "@ROOT@/cap/battery"
CMDS = [l.strip() for l in open(sys.argv[1]) if l.strip() and not l.startswith("#")]
def norm(s):
    s = re.sub(r"\x1b\[[0-9;?]*[a-zA-Z]", "", s)
    s = s.replace("HERMES", "ICEBATH").replace("Hermes", "Icebath").replace("hermes-agent", "icebath").replace("hermes", "icebath")
    s = s.replace("@ROOT@/ref-home", "<HOME>").replace("@ROOT@/ib-home", "<HOME>")
    s = s.replace("@ROOT@/uh-ref", "<UH>").replace("@ROOT@/uh-ib", "<UH>")
    s = re.sub(r"\d{4}-\d\d-\d\d[ T]\d\d:\d\d(:\d\d)?(\.\d+)?", "<TS>", s)
    s = re.sub(r"\b\d{8}_\d{6}_[0-9a-f]+\b", "<SID>", s)
    s = re.sub(r"[ \t]+\n", "\n", s)
    return s
def run(which, cmd):
    env = dict(os.environ, HOME="@ROOT@/uh-" + which, NO_COLOR="1", COLUMNS="120", LINES="40", TERM="xterm-256color")
    t = time.time()
    try:
        p = subprocess.run(["@ROOT@/" + which] + cmd.split(), env=env, cwd="@ROOT@/work", stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=60)
        rc, o, e = p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired as x:
        rc, o, e = "TO", (x.stdout or b"").decode(errors="replace") if isinstance(x.stdout, bytes) else (x.stdout or ""), ""
    return rc, o, e, time.time() - t
rows = []
for i, c in enumerate(CMDS):
    r = run("ref", c); b = run("ib", c)
    tag = "%03d_%s" % (i, re.sub(r"\W+", "_", c)[:50])
    ro, bo = norm(r[1]), norm(b[1]); re_, be = norm(r[2]), norm(b[2])
    open(f"{OUT}/{tag}.ref.txt", "w").write(ro + "\n--STDERR--\n" + re_)
    open(f"{OUT}/{tag}.ib.txt", "w").write(bo + "\n--STDERR--\n" + be)
    d = [l for l in difflib.unified_diff(ro.split("\n"), bo.split("\n"), lineterm="", n=0) if l[:1] in "+-" and l[:3] not in ("+++", "---")]
    de = [l for l in difflib.unified_diff(re_.split("\n"), be.split("\n"), lineterm="", n=0) if l[:1] in "+-" and l[:3] not in ("+++", "---")]
    rows.append((c, r[0], b[0], len(ro.split("\n")), len(bo.split("\n")), len(d), len(de), r[3], b[3], tag))
    print("%-44s rc %s/%s lines %d/%d diff %d err %d t %.2f/%.2f" % (c[:44], r[0], b[0], rows[-1][3], rows[-1][4], len(d), len(de), r[3], b[3]), flush=True)
json.dump(rows, open(OUT + "/summary.json", "w"))
