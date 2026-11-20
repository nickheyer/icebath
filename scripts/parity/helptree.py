import re, subprocess, sys, os, json, time
from concurrent.futures import ThreadPoolExecutor

exe, outdir = sys.argv[1], sys.argv[2]
os.makedirs(outdir, exist_ok=True)
CHOICES = re.compile(r"\{([A-Za-z0-9_,\-]+)\}")

def run(path):
    t = time.time()
    try:
        p = subprocess.run([exe, *path, "--help"], capture_output=True, text=True, timeout=60,
                           stdin=subprocess.DEVNULL, env={**os.environ, "COLUMNS": "80", "NO_COLOR": "1", "TERM": "dumb"})
        out, err, rc = p.stdout, p.stderr, p.returncode
    except subprocess.TimeoutExpired:
        out, err, rc = "", "TIMEOUT", 124
    return path, out, err, rc, time.time() - t

def subcommands(out):
    usage = out.split("\n\n", 1)[0]
    names = []
    for m in CHOICES.finditer(usage):
        for n in m.group(1).split(","):
            if n and n not in names:
                names.append(n)
    listed = []
    for line in out.splitlines():
        m = re.match(r"^    ([a-z][a-z0-9_\-]+)(\s{2,}.*)?$", line)
        if m:
            listed.append(m.group(1))
    return names, listed

index = {}
frontier = [[]]
seen = set()
with ThreadPoolExecutor(max_workers=8) as pool:
    while frontier:
        results = list(pool.map(run, frontier))
        frontier = []
        for path, out, err, rc, dt in results:
            key = " ".join(path) or "(root)"
            fn = "_".join(path) or "root"
            with open(os.path.join(outdir, fn + ".txt"), "w") as f:
                f.write(out)
                if err.strip():
                    f.write("\n--- stderr ---\n" + err)
            index[key] = {"rc": rc, "secs": round(dt, 3), "lines": out.count("\n")}
            if rc != 0 or len(path) >= 4:
                continue
            names, listed = subcommands(out)
            cands = names if path else listed
            for n in cands:
                child = path + [n]
                k = tuple(child)
                if k not in seen:
                    seen.add(k)
                    frontier.append(child)
with open(os.path.join(outdir, "_index.json"), "w") as f:
    json.dump(index, f, indent=1, sort_keys=True)
print(len(index), "help pages")
