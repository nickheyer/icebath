import json, sys
# usage: mkstart.py <ib|ref> <home-suffix> <name> [args...]  builds a classic CLI startup spec with the homes/<suffix>-<side> home
which, suf, name = sys.argv[1], sys.argv[2], sys.argv[3]
extra = sys.argv[4:]
L = "@ROOT@"
env = {"HOME": L + "/uh-" + which, ("ICEBATH_HOME" if which == "ib" else "HERMES_HOME"): L + "/homes/" + suf + "-" + which}
steps = [["quiet", 2.0, 45], ["snap", "01_start"], ["type", "/exit"], ["send", "\r"], ["quiet", 1.0, 15], ["snap", "02_exit"]]
json.dump({"argv": [L + "/" + which] + extra, "env": env, "rows": 50, "cols": 140, "cwd": L + "/work", "steps": steps}, open(L + "/cap/spec_%s.json" % name, "w"))
