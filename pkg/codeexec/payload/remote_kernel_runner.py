"""Auto-generated Icebath REMOTE session-kernel runner (file cell protocol)."""
import contextlib
import io
import json
import os
import sys
import time
import traceback

KDIR = os.environ["ICEBATH_KERNEL_DIR"]
CELLS = os.path.join(KDIR, "cells")
_CAPTURE_LIMIT = {capture_limit}
IDLE_EXIT_SECONDS = {idle_exit}

{cell_source}

def main():
    execution_count = 0
    last_activity = time.time()
    while True:
        pending = sorted(
            f for f in os.listdir(CELLS)
            if f.startswith("cell_req_") and f.endswith(".json")
        )
        if not pending:
            if time.time() - last_activity > IDLE_EXIT_SECONDS:
                return  # self-reap: nobody is talking to us anymore
            time.sleep(0.2)
            continue
        for name in pending:
            req_path = os.path.join(CELLS, name)
            try:
                with open(req_path, "r", encoding="utf-8") as f:
                    request = json.load(f)
            except Exception:
                # Partially-written request (ship in progress): retry next tick.
                continue
            os.remove(req_path)
            last_activity = time.time()
            execution_count += 1
            payload, _ = run_cell(request, execution_count)
            res_name = name.replace("cell_req_", "cell_res_")
            tmp = os.path.join(CELLS, res_name + ".tmp")
            # Cell results carry the executed code's output: owner-only, even if
            # the process umask is permissive.
            fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(payload, f, ensure_ascii=False)
            os.replace(tmp, os.path.join(CELLS, res_name))
            if payload["status"] == "exit":
                return


if __name__ == "__main__":
    main()
