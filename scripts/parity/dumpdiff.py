import json, sys, difflib, re, os
a = json.load(open(sys.argv[1])); b = json.load(open(sys.argv[2]))
KEPT = "hermes-agent.nousresearch.com"
FITTED = [("Use, configure, theme, extend, and orchestrate Icebath Agent.", "Use, configure, theme, extend and orchestrate Icebath Agent.")]
def norm(s):
    s = s.replace(KEPT, "\0KEPT\0")
    s = s.replace("hermes", "icebath").replace("Hermes", "Icebath").replace("HERMES", "ICEBATH")
    s = s.replace("\0KEPT\0", KEPT)
    for a, b in FITTED:
        s = s.replace(a, b)
    s = re.sub(r"/ref-([a-z-]*home)\b", r"/ib-\1", s).replace("uh-ref", "uh-ib")
    return s
def show(title, x, y):
    x, y = norm(x), y
    if x == y:
        print("== %s: IDENTICAL (%d chars)" % (title, len(y))); return
    print("== %s: DIFFERENT (ref %d chars, ib %d chars)" % (title, len(x), len(y)))
    for line in list(difflib.unified_diff(x.splitlines(), y.splitlines(), "ref", "ib", lineterm="", n=1))[:int(sys.argv[3]) if len(sys.argv) > 3 else 80]:
        print(line[:400])
ha, hb = a["headers"], b["headers"]
print("== headers ref:", json.dumps(ha, sort_keys=True)); print("== headers ib :", json.dumps(hb, sort_keys=True))
if "header_lines" in a and "header_lines" in b:
    print("== http version ref=%s ib=%s" % (a.get("request_version"), b.get("request_version")))
    show("header lines in wire order", "\n".join(a["header_lines"]), "\n".join(b["header_lines"]))
ba, bb = a["body"], b["body"]
print("== body keys ref:", sorted(ba)); print("== body keys ib :", sorted(bb))
for k in sorted(set(ba) | set(bb)):
    if k in ("messages", "tools"): continue
    if ba.get(k) != bb.get(k): print("== key %s differs: ref=%r ib=%r" % (k, ba.get(k), bb.get(k)))
ma, mb = ba.get("messages", []), bb.get("messages", [])
print("== message count ref=%d ib=%d" % (len(ma), len(mb)))
for i in range(max(len(ma), len(mb))):
    x = ma[i] if i < len(ma) else {}; y = mb[i] if i < len(mb) else {}
    cx = x.get("content"); cy = y.get("content")
    cx = cx if isinstance(cx, str) else json.dumps(cx, indent=1, sort_keys=True, ensure_ascii=False)
    cy = cy if isinstance(cy, str) else json.dumps(cy, indent=1, sort_keys=True, ensure_ascii=False)
    show("message[%d] role=%s/%s" % (i, x.get("role"), y.get("role")), cx, cy)
    rx = {k: v for k, v in x.items() if k != "content"}; ry = {k: v for k, v in y.items() if k != "content"}
    if norm(json.dumps(rx, sort_keys=True)) != json.dumps(ry, sort_keys=True):
        show("message[%d] other fields" % i, json.dumps(rx, indent=1, sort_keys=True), json.dumps(ry, indent=1, sort_keys=True))
ta = {t["function"]["name"]: t for t in ba.get("tools") or []}; tb = {t["function"]["name"]: t for t in bb.get("tools") or []}
print("== tool order ref:", [t["function"]["name"] for t in ba.get("tools") or []]); print("== tool order ib :", [t["function"]["name"] for t in bb.get("tools") or []])
for n in sorted(set(ta) | set(tb)):
    show("tool " + n, json.dumps(ta.get(n), indent=1, ensure_ascii=False), json.dumps(tb.get(n), indent=1, ensure_ascii=False))
ra, rb = sys.argv[1][:-5] + ".raw", sys.argv[2][:-5] + ".raw"
if os.path.exists(ra) and os.path.exists(rb):
    xa, xb = norm(open(ra, encoding="utf-8").read()), open(rb, encoding="utf-8").read()
    print("== raw body: %s (ref %d chars, ib %d chars)" % ("IDENTICAL" if xa == xb else "DIFFERENT", len(xa), len(xb)))
    orders = []
    def keyorder(x, y, p):
        if isinstance(x, list) and isinstance(y, list) and x and y and isinstance(x[0], tuple) and isinstance(y[0], tuple):
            kx, ky = [k for k, _ in x], [k for k, _ in y]
            if kx != ky and sorted(kx) == sorted(ky):
                orders.append("%s ref=%s ib=%s" % (p or "/", kx, ky))
            dy = dict(y)
            for k, v in x:
                if k in dy:
                    keyorder(v, dy[k], p + "/" + k)
        elif isinstance(x, list) and isinstance(y, list):
            for i, (u, v) in enumerate(zip(x, y)):
                keyorder(u, v, "%s[%d]" % (p, i))
    pairs = lambda s: json.loads(s, object_pairs_hook=lambda kv: [tuple(e) for e in kv] or [("", None)])
    keyorder(pairs(xa), pairs(xb), "")
    print("== key order differences: %d" % len(orders))
    for line in orders[:int(sys.argv[3]) if len(sys.argv) > 3 else 80]:
        print(line[:400])
