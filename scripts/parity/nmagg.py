import sys, re, collections
by = collections.Counter(); byt = collections.Counter()
for line in open(sys.argv[1]):
    p = line.split(None, 3)
    if len(p) < 4:
        continue
    try:
        size = int(p[1])
    except ValueError:
        continue
    typ, name = p[2], p[3].strip()
    if typ in "bB":
        continue
    name = re.sub(r"^type:\.eq\.|^type:\.hash\.|^go:itab\.\*?|^type:", "", name)
    name = re.sub(r"\[.*", "", name)
    m = re.match(r"((?:[\w.\-~]+/)*[\w\-~]+)\.", name)
    pkg = m.group(1) if m else name[:40]
    depth = int(sys.argv[2]) if len(sys.argv) > 2 else 4
    parts = pkg.split("/")
    key = "/".join(parts[:depth])
    by[key] += size
tot = sum(by.values())
print("total", tot)
for k, v in by.most_common(int(sys.argv[3]) if len(sys.argv) > 3 else 60):
    print("%10d %5.1f%% %s" % (v, 100.0 * v / tot, k))
