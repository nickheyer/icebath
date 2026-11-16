import os, pty, sys, time, json, re, select, struct, fcntl, termios, signal
import pyte

def main():
    spec = json.load(open(sys.argv[1]))
    outdir = sys.argv[2]
    os.makedirs(outdir, exist_ok=True)
    rows, cols = spec.get("rows", 40), spec.get("cols", 120)
    env = dict(os.environ)
    env.update({"TERM": "xterm-256color", "COLORTERM": "truecolor", "LINES": str(rows), "COLUMNS": str(cols), "LANG": "C.UTF-8"})
    env.pop("NO_COLOR", None)
    env.update(spec.get("env", {}))
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(spec.get("cwd", "@ROOT@/work"))
        os.execvpe(spec["argv"][0], spec["argv"], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    screen = pyte.Screen(cols, rows)
    stream = pyte.ByteStream(screen)
    screen.write_process_input = lambda data: os.write(fd, data.encode())
    raw = open(os.path.join(outdir, "raw.bin"), "wb")
    timeline = open(os.path.join(outdir, "timeline.jsonl"), "w")
    t0 = time.time()
    stats = {"bytes": 0, "writes": 0}
    alive = [True]

    def pump(timeout):
        end = time.time() + timeout
        got = False
        while True:
            left = end - time.time()
            if left < 0:
                left = 0
            try:
                r, _, _ = select.select([fd], [], [], left)
            except OSError:
                alive[0] = False
                return got
            if not r:
                return got
            try:
                data = os.read(fd, 65536)
            except OSError:
                alive[0] = False
                return got
            if not data:
                alive[0] = False
                return got
            raw.write(data); raw.flush()
            stats["bytes"] += len(data); stats["writes"] += 1
            timeline.write(json.dumps({"t": round(time.time() - t0, 4), "n": len(data)}) + "\n")
            stream.feed(data)
            got = True
            if time.time() >= end:
                return got

    def text():
        return "\n".join(line.rstrip() for line in screen.display)

    def attrs():
        out = []
        for y in range(screen.lines):
            row = screen.buffer[y]
            runs = []
            cur = None
            for x in range(screen.columns):
                c = row[x]
                key = (c.fg, c.bg, c.bold, c.italics, c.underscore, c.reverse, c.strikethrough)
                if cur and cur[0] == key:
                    cur[1] += c.data
                else:
                    cur = [key, c.data]
                    runs.append(cur)
            out.append([[list(k), t] for k, t in runs if t.strip() or k[1] != "default" or k[5]])
        return out

    report = []
    for step in spec["steps"]:
        kind = step[0]
        started = time.time()
        if kind == "wait":
            pat, limit = re.compile(step[1], re.S), step[2]
            ok = False
            while time.time() - started < limit and alive[0]:
                pump(0.05)
                if pat.search(text()):
                    ok = True
                    break
            report.append({"step": step, "ok": ok, "secs": round(time.time() - started, 3), "at": round(time.time() - t0, 3)})
        elif kind == "quiet":
            idle, limit = step[1], step[2]
            last = time.time()
            while time.time() - started < limit and alive[0]:
                if pump(0.05):
                    last = time.time()
                elif time.time() - last >= idle:
                    break
            report.append({"step": step, "secs": round(time.time() - started, 3), "at": round(time.time() - t0, 3)})
        elif kind == "send":
            os.write(fd, step[1].encode())
            report.append({"step": step, "at": round(time.time() - t0, 3)})
        elif kind == "type":
            for ch in step[1]:
                os.write(fd, ch.encode()); pump(step[2] if len(step) > 2 else 0.01)
            report.append({"step": [kind, step[1][:40]], "secs": round(time.time() - started, 3), "at": round(time.time() - t0, 3)})
        elif kind == "sleep":
            pump(step[1])
        elif kind == "resize":
            rows, cols = step[1], step[2]
            fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
            screen.resize(rows, cols)
            os.kill(pid, signal.SIGWINCH)
            report.append({"step": step, "at": round(time.time() - t0, 3)})
        elif kind == "snap":
            with open(os.path.join(outdir, step[1] + ".txt"), "w") as f:
                f.write(text() + "\n")
            with open(os.path.join(outdir, step[1] + ".attrs.json"), "w") as f:
                json.dump(attrs(), f)
            report.append({"step": step, "at": round(time.time() - t0, 3), "bytes": stats["bytes"], "writes": stats["writes"], "cursor": [screen.cursor.y, screen.cursor.x]})
        elif kind == "mark":
            report.append({"step": step, "at": round(time.time() - t0, 3), "bytes": stats["bytes"], "writes": stats["writes"]})
    try:
        os.kill(pid, signal.SIGTERM)
        time.sleep(0.3)
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass
    try:
        os.waitpid(pid, 0)
    except OSError:
        pass
    json.dump({"report": report, "stats": stats, "alive_at_end": alive[0]}, open(os.path.join(outdir, "report.json"), "w"), indent=1)
    for r in report:
        print(json.dumps(r))

main()
