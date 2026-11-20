#!/usr/bin/env python3
# Prints bytes written per step for two xdrive2 capture directories side by side
import json, sys
a, b = sys.argv[1], sys.argv[2]
def key(e): return e.get('snap') or e.get('wait') or e.get('mark') or e.get('waitgone')
A = json.load(open(a + '/report.json'))['report']; B = {key(e): e for e in json.load(open(b + '/report.json'))['report']}
pa = pb = 0
for e in A:
    k = key(e); o = B.get(k)
    if not o: continue
    print(f"{k[:30]:30s} {e['bytes']-pa:8d} {o['bytes']-pb:8d}  at {e['at']:7.2f} {o['at']:7.2f}")
    pa, pb = e['bytes'], o['bytes']
print(f"{'total':30s} {pa:8d} {pb:8d}")
