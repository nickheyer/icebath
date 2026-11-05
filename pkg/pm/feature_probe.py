
import contextlib, importlib.util, json, os, site, sys, sysconfig
base, anchors = sys.argv[1], json.loads(sys.argv[2])
paths = sysconfig.get_paths(vars={"base": base, "platbase": base})
sites = dict.fromkeys(paths[key] for key in ("purelib", "platlib"))
if not any(os.path.isdir(path) for path in sites):
    raise RuntimeError("target dependency tree has no site-packages")
result = {}
with contextlib.redirect_stdout(sys.stderr):
    for path in sites:
        site.addsitedir(path)
    for anchor in anchors:
        try:
            result[anchor] = importlib.util.find_spec(anchor) is not None
        except (ImportError, ValueError):
            result[anchor] = False
print(json.dumps(result))
