package plugins

import ()

const hostStatePlugin = `
import json, os
def register(ctx):
    def run(args, **kw):
        st = ctx.state
        st.set("count", {"n": 3})
        out = {"pid": os.getpid(), "got": st.get("count"), "missing": st.get("nope", default="dflt"),
               "dir": st.data_dir, "path": st.path, "quota": st.quota_bytes}
        for name, probe in (("bad_key", lambda: st.set("a..b", 1)), ("attr", lambda: st.nope)):
            try:
                probe()
            except Exception as exc:
                out[name] = type(exc).__name__ + ": " + str(exc)
        return json.dumps(out)
    ctx.register_tool(name="hoststate", toolset="hoststate",
        schema={"name": "hoststate", "description": "d", "parameters": {"type": "object", "properties": {}}}, handler=run)
`
