# Diffs two fakeha logs request by request, headers included except the random WebSocket key
import json, sys
SKIP = {"sec-websocket-key"}
def load(p):
    rows = []
    for line in open(p):
        e = json.loads(line)
        if "ws" in e:
            rows.append(("WS " + e["ws"], e["path"], json.dumps(e.get("data"), sort_keys=True), []))
            continue
        try:
            body = json.dumps(json.loads(e["body"]), sort_keys=True) if e["body"] else ""
        except ValueError:
            body = e["body"]
        hdr = sorted((k.lower(), v) for k, v in e["headers"] if k.lower() not in SKIP)
        rows.append((e["method"] + " " + e["version"], e["path"], body, hdr))
    return rows
a, b = load(sys.argv[1]), load(sys.argv[2])
same = True
print("requests ref=%d ib=%d" % (len(a), len(b)))
for i in range(max(len(a), len(b))):
    x = a[i] if i < len(a) else ("-", "-", "", []); y = b[i] if i < len(b) else ("-", "-", "", [])
    if x == y:
        print("[%d] SAME %s %s" % (i, x[0], x[1])); continue
    same = False
    print("[%d] DIFF ref %s %s | ib %s %s" % (i, x[0], x[1], y[0], y[1]))
    if x[2] != y[2]: print("   body ref %s\n   body ib  %s" % (x[2], y[2]))
    hx, hy = dict(x[3]), dict(y[3])
    for k in sorted(set(hx) | set(hy)):
        if hx.get(k) != hy.get(k): print("   header %s ref=%r ib=%r" % (k, hx.get(k), hy.get(k)))
print("IDENTICAL" if same else "DIFFERENT")
