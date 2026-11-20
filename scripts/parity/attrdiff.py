import json, sys, os
a_dir, b_dir = sys.argv[1], sys.argv[2]
limit = int(sys.argv[3]) if len(sys.argv) > 3 else 12
names = sorted(f[:-11] for f in os.listdir(a_dir) if f.endswith(".attrs.json"))
def fmt(k):
    fg, bg, bold, it, ul, rev, st = k
    return "fg=%s bg=%s%s%s%s%s%s" % (fg, bg, " bold" if bold else "", " ital" if it else "", " ul" if ul else "", " rev" if rev else "", " strike" if st else "")
for n in names:
    A = json.load(open(os.path.join(a_dir, n + ".attrs.json"))); B = json.load(open(os.path.join(b_dir, n + ".attrs.json")))
    shown = 0; total = 0
    for y in range(min(len(A), len(B))):
        ra = [(fmt(k), t.strip()) for k, t in A[y] if t.strip()]; rb = [(fmt(k), t.strip()) for k, t in B[y] if t.strip()]
        ta = " ".join(t for _, t in ra); tb = " ".join(t for _, t in rb)
        sa = [k for k, _ in ra]; sb = [k for k, _ in rb]
        if sa != sb:
            total += 1
            if shown < limit:
                shown += 1
                if shown == 1: print("=====", n)
                print(" row %d text-same=%s" % (y, ta.replace("Hermes", "Icebath").replace("hermes", "icebath") == tb))
                print("   ref:", [(k, t[:28]) for k, t in ra][:5])
                print("   ib :", [(k, t[:28]) for k, t in rb][:5])
    if total: print("  -> %s: %d rows with style differences" % (n, total))
