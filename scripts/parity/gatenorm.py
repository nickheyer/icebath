# Extracts plugin gate decisions and discovery totals from an agent.log into sorted normalized lines
import re, sys
out = set()
for line in open(sys.argv[1], errors="replace"):
    m = re.search(r"Loading plugin '([^']+)'", line)
    if m: out.add("load " + m.group(1)); continue
    m = re.search(r"Skipping '([^']+)' \((.*)\)", line)
    if m: out.add("skip %s (%s)" % (m.group(1), m.group(2).replace("hermes", "icebath"))); continue
    m = re.search(r"Registered deferred platform loader: (\S+) \(plugin=([^)]+)\)", line)
    if m: out.add("deferred " + m.group(2)); continue
    m = re.search(r"Plugin discovery complete: (\d+) found, (\d+) enabled", line)
    if m: out.add("complete found=%s enabled=%s" % m.groups()); continue
    m = re.search(r"Plugin '([^']+)' skipped: (.*)", line)
    if m: out.add("skipped %s: %s" % (m.group(1), m.group(2).strip())); continue
    m = re.search(r"Failed to load plugin '([^']+)'", line)
    if m: out.add("failed " + m.group(1))
for l in sorted(out): print(l)
