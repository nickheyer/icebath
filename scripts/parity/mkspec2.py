import json, sys
which, name = sys.argv[1], sys.argv[2]
argv = ["@ROOT@/" + which] + sys.argv[3:]
env = {"HOME": "@ROOT@/uh-" + which}
PASTE = "\x1b[200~" + "\n".join("pasted line %d with some text" % i for i in range(1, 31)) + "\x1b[201~"
steps = [
    ["quiet", 1.5, 30], ["snap", "01_start"],
    ["type", "line one"], ["send", "\n"], ["type", "line two"], ["sleep", 0.3], ["snap", "02_multiline"], ["send", "\r"], ["quiet", 2.0, 40], ["snap", "03_multiline_sent"],
    ["send", "\x1b[A"], ["sleep", 0.4], ["snap", "04_history_up"], ["send", "\x15"], ["send", "\x03"], ["sleep", 0.4], ["snap", "05_after_clear"],
    ["type", "/"], ["sleep", 0.5], ["snap", "06_slash_menu"], ["type", "mod"], ["sleep", 0.5], ["snap", "07_slash_mod"], ["send", "\t"], ["sleep", 0.4], ["snap", "08_slash_tab"],
    ["send", "\r"], ["quiet", 1.5, 20], ["snap", "09_model_cmd"], ["send", "\x1b"], ["sleep", 0.6], ["send", "\x15"], ["sleep", 0.3], ["snap", "10_after_esc"],
    ["type", "look at @al"], ["sleep", 0.6], ["snap", "11_at_completion"], ["send", "\x15"], ["sleep", 0.3],
    ["type", "!echo shell-says-hi"], ["send", "\r"], ["quiet", 1.5, 20], ["snap", "12_bang_shell"],
    ["send", "\x10"], ["sleep", 0.6], ["snap", "13_ctrl_p_palette"], ["send", "\x1b"], ["sleep", 0.5], ["snap", "14_palette_closed"],
    ["type", "write a long answer"], ["send", "\r"], ["sleep", 1.5], ["type", "redirect now"], ["sleep", 0.3], ["snap", "15_typed_during_stream"], ["send", "\r"], ["quiet", 2.0, 60], ["snap", "16_after_redirect"],
    ["type", "write a long answer"], ["send", "\r"], ["sleep", 1.5], ["send", "\x03"], ["quiet", 1.5, 30], ["snap", "17_after_ctrl_c"],
    ["type", "usedanger now"], ["send", "\r"], ["quiet", 2.0, 40], ["snap", "18_approval_prompt"], ["send", "\x1b[B"], ["sleep", 0.3], ["snap", "19_approval_down"], ["send", "\x1b"], ["quiet", 2.0, 40], ["snap", "20_approval_resolved"],
    ["send", PASTE], ["sleep", 0.8], ["snap", "21_pasted"], ["send", "\x15"], ["send", "\x03"], ["sleep", 0.5], ["snap", "22_paste_cleared"],
    ["type", "/status"], ["send", "\r"], ["quiet", 1.5, 20], ["snap", "23_status"],
    ["type", "/usage"], ["send", "\r"], ["quiet", 1.5, 20], ["snap", "24_usage"],
    ["send", "\x04"], ["quiet", 1.0, 15], ["snap", "25_ctrl_d"],
]
json.dump({"argv": argv, "env": env, "rows": 40, "cols": 120, "cwd": "@ROOT@/work", "steps": steps}, open("@ROOT@/cap/spec_%s.json" % name, "w"))
