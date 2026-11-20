#!/usr/bin/env python3
# Prints write count, sizes, cadence and one sample frame between two named steps of an xdrive2 capture
import json, re, sys
d, a, b = sys.argv[1], sys.argv[2], sys.argv[3]
pick = float(sys.argv[4]) if len(sys.argv) > 4 else 0.5
raw = open(d + '/raw.bin', 'rb').read()
tl = [json.loads(l) for l in open(d + '/timeline.jsonl')]
rep = json.load(open(d + '/report.json'))['report']
def at(name):
    for e in rep:
        if (e.get('snap') or e.get('wait') or e.get('mark')) == name: return e['bytes'], e['at']
b0, t0 = at(a); b1, t1 = at(b)
off = 0; seg = []
for w in tl:
    if b0 <= off < b1: seg.append((w['t'], off, w['n']))
    off += w['n']
sizes = sorted(n for _, _, n in seg)
print('writes', len(seg), 'bytes', b1 - b0, 'secs', round(t1 - t0, 2), 'median', sizes[len(sizes) // 2], 'p90', sizes[int(len(sizes) * 0.9)], 'max', sizes[-1])
t, o, n = seg[int(len(seg) * pick)]
fr = raw[o:o + n].decode('utf8', 'replace')
plain = re.sub(r'\x1b\[[0-9;:?]*[A-Za-z]', '', fr)
print('frame', n, 'sgr', len(re.findall(r'\x1b\[[0-9;:]*m', fr)), 'cup', len(re.findall(r'\x1b\[[0-9;]*[HGdABCD]', fr)), 'printable', len(plain), 'newlines', fr.count('\n'))
print('other', sorted(set(re.findall(r'\x1b\[[0-9;:?]*[A-Za-ln-z]', fr)))[:30])
print(repr(fr[:int(sys.argv[5]) if len(sys.argv) > 5 else 500]))
