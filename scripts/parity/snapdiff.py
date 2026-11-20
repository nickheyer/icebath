import sys, os, difflib, re
a, b = sys.argv[1], sys.argv[2]
names = sys.argv[3].split(",") if len(sys.argv) > 3 and sys.argv[3] else sorted(f[:-4] for f in os.listdir(a) if f.endswith(".txt") and not f.endswith(".all.txt"))
ctx = int(sys.argv[4]) if len(sys.argv) > 4 else 0
def norm(s, ref):
    if ref:
        s = s.replace("Hermes", "Icebath").replace("hermes", "icebath").replace("HERMES", "ICEBATH")
    s = re.sub(r"local-[0-9a-f]{64}", "local-ID", s)
    s = re.sub(r"Session: [0-9a-f]{8}", "Session: ID", s)
    s = re.sub(r"\b\d+(\.\d+)?s\b", "Ns", s); s = re.sub(r"\d+ t/s", "N t/s", s); s = re.sub(r"[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]", "*", s)
    s = re.sub(r"#\d+ ─", "#N ─", s); s = re.sub(r"\d+:\d\d [AP]M", "TIME", s)
    return [l.rstrip() for l in s.splitlines()]
for n in names:
    A = norm(open(os.path.join(a, n + ".txt")).read(), True); B = norm(open(os.path.join(b, n + ".txt")).read(), False)
    while A and not A[-1]: A.pop()
    while B and not B[-1]: B.pop()
    d = [l for l in difflib.unified_diff(A, B, "ref", "ib", lineterm="", n=ctx)][2:]
    ch = sum(1 for l in d if l[:1] in "+-")
    print("== %s: %s" % (n, "SAME" if not ch else "%d changed lines" % ch))
    for l in d[:int(sys.argv[5]) if len(sys.argv) > 5 else 14]:
        print("   " + l[:200])
