import json, sys
which, name = sys.argv[1], sys.argv[2]
extra = sys.argv[3:]
argv = ["@ROOT@/" + which] + extra
env = {"HOME": "@ROOT@/uh-" + which}
steps = [
    ["quiet", 1.5, 30], ["snap", "01_start"],
    ["type", "Say hello"], ["sleep", 0.3], ["snap", "02_typed"], ["send", "\r"],
    ["quiet", 2.0, 40], ["snap", "03_hello"],
    ["type", "show markdown"], ["send", "\r"], ["sleep", 0.6], ["snap", "04_md_streaming"],
    ["quiet", 2.0, 60], ["snap", "05_md_done"],
    ["type", "usetool please"], ["send", "\r"], ["sleep", 0.8], ["snap", "06_tool_running"],
    ["quiet", 2.5, 60], ["snap", "07_tool_done"],
    ["type", "/help"], ["sleep", 0.5], ["snap", "08_slash_typed"], ["send", "\r"],
    ["quiet", 1.5, 30], ["snap", "09_help"],
    ["type", "think about it"], ["send", "\r"], ["quiet", 2.0, 40], ["snap", "10_think"],
    ["resize", 30, 80], ["quiet", 1.0, 10], ["snap", "11_resized_80x30"],
    ["type", "write a long answer"], ["send", "\r"], ["sleep", 1.0], ["snap", "12_long_streaming"], ["quiet", 2.0, 90], ["snap", "13_long_done"],
    ["resize", 40, 120], ["quiet", 1.0, 10], ["snap", "14_resized_back"],
    ["type", "/exit"], ["send", "\r"], ["quiet", 1.0, 15], ["snap", "15_exit"],
]
json.dump({"argv": argv, "env": env, "rows": 40, "cols": 120, "cwd": "@ROOT@/work", "steps": steps}, open("@ROOT@/cap/spec_%s.json" % name, "w"))
