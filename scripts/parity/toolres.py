# Compares the tool result messages of two model request dumps
import json, sys
def res(p):
    return [(m.get("tool_call_id"), m.get("content")) for m in json.load(open(p))["body"]["messages"] if m.get("role") == "tool"]
a, b = res(sys.argv[1]), res(sys.argv[2])
print("tool results ref=%d ib=%d" % (len(a), len(b)))
for i in range(max(len(a), len(b))):
    x = a[i] if i < len(a) else None; y = b[i] if i < len(b) else None
    print("[%d] %s" % (i, "SAME" if x == y else "DIFF"))
    if x != y: print("   ref %r\n   ib  %r" % (x, y))
    else: print("   %s" % (x[1][:200],))
