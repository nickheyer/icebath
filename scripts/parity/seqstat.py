import re, sys, collections
for p in sys.argv[1:]:
    b = open(p, "rb").read()
    c = collections.Counter()
    for m in re.finditer(rb"\x1b\[([?>=]?)([0-9;]*)([a-zA-Z])", b):
        k = m.group(1).decode() + m.group(3).decode()
        if k in ("m",): continue
        if k in ("?h", "?l", "n", "J", "K", "H", "c"): k = m.group(1).decode() + m.group(2).decode() + m.group(3).decode()
        c[k] += 1
    print(p, len(b)); print("  ", dict(sorted(c.items())))
