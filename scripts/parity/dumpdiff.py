import json, sys, difflib, re
a = json.load(open(sys.argv[1])); b = json.load(open(sys.argv[2]))
def norm(s):
    s = s.replace("hermes", "icebath").replace("Hermes", "Icebath").replace("HERMES", "ICEBATH")
    s = s.replace("ref-home", "ib-home").replace("uh-ref", "uh-ib")
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
