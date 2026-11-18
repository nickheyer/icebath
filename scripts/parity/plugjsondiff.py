# Compares plugins list --json rows of both sides by name and prints every differing field
import json, sys
a = {r["name"]: r for r in json.load(open(sys.argv[1]))}
b = {r["name"]: r for r in json.load(open(sys.argv[2]))}
print("ref rows %d ib rows %d" % (len(a), len(b)))
for n in sorted(set(a) | set(b)):
    if n not in b: print("ONLY REF", n, a[n]["status"]); continue
    if n not in a: print("ONLY IB ", n, b[n]["status"], b[n]["source"]); continue
    for k in sorted(set(a[n]) | set(b[n])):
        x, y = a[n].get(k), b[n].get(k)
        if isinstance(x, str): x = x.replace("a Hermes", "an Icebath").replace("hermes", "icebath").replace("Hermes", "Icebath").replace("HERMES", "ICEBATH")
        if x != y: print("DIFF %s %s\n   ref %r\n   ib  %r" % (n, k, a[n].get(k), y))
